package splunk

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
	"github.com/stripe/veneur/trace/metrics"
)

// TestableSplunkSpanSink provides methods that are useful for testing
// a splunk span sink.
type TestableSplunkSpanSink interface {
	sinks.SpanSink

	// Stop shuts down the sink's submission workers by finishing
	// each worker's last submission HTTP request.
	Stop()

	// Sync instructs all submission workers to finish submitting
	// their current request and start a new one. It returns when
	// the last worker's submission is done.
	Sync()
}

type splunkSpanSink struct {
	hec           *hecClient
	httpClient    *http.Client
	hostname      string
	sendTimeout   time.Duration
	ingestTimeout time.Duration

	workers int

	batchSize            int
	hecSubmissionWorkers int
	ingestedSpans        uint32
	droppedSpans         uint32

	ingest chan *Event

	traceClient *trace.Client
	log         *logrus.Logger

	spanSampleRate int64
	skippedSpans   uint32

	// these fields are for testing only:

	// sync holds one channel per submission worker.
	sync []chan struct{}

	// synced is marked Done by each submission worker, when the
	// submission has happened.
	synced sync.WaitGroup
}

var _ sinks.SpanSink = &splunkSpanSink{}
var _ TestableSplunkSpanSink = &splunkSpanSink{}

// NewSplunkSpanSink constructs a new splunk span sink from the server
// name and token provided, using the local hostname configured for
// veneur. An optional argument, validateServerName is used (if
// non-empty) to instruct go to validate a different hostname than the
// one on the server URL.
// The spanSampleRate is an integer. For any given trace ID, the probability
// that all spans in the trace will be chosen for the sample is 1/spanSampleRate.
// Sampling is performed on the trace ID, so either all spans within a given trace
// will be chosen, or none will.
func NewSplunkSpanSink(server string, token string, localHostname string, validateServerName string, log *logrus.Logger, ingestTimeout time.Duration, sendTimeout time.Duration, batchSize int, workers int, spanSampleRate int) (sinks.SpanSink, error) {
	if spanSampleRate < 1 {
		spanSampleRate = 1
	}

	client, err := newHecClient(server, token)
	if err != nil {
		return nil, err
	}

	trnsp := &http.Transport{}
	httpC := &http.Client{Transport: trnsp}

	// keep an idle connection in reserve for every worker:
	trnsp.MaxIdleConnsPerHost = workers

	if validateServerName != "" {
		tlsCfg := &tls.Config{}
		tlsCfg.ServerName = validateServerName
		trnsp.TLSClientConfig = tlsCfg
	}
	if sendTimeout > 0 {
		trnsp.ResponseHeaderTimeout = sendTimeout
	}

	return &splunkSpanSink{
		hec:            client,
		httpClient:     httpC,
		ingest:         make(chan *Event),
		hostname:       localHostname,
		log:            log,
		sendTimeout:    sendTimeout,
		ingestTimeout:  ingestTimeout,
		batchSize:      batchSize,
		spanSampleRate: int64(spanSampleRate),
	}, nil
}

// Name returns this sink's name
func (*splunkSpanSink) Name() string {
	return "splunk"
}

func (sss *splunkSpanSink) Start(cl *trace.Client) error {
	sss.traceClient = cl

	workers := 1
	if sss.workers > 0 {
		workers = sss.workers
	}

	sss.sync = make([]chan struct{}, workers)

	for i := 0; i < workers; i++ {
		ch := make(chan struct{})
		go sss.submitter(ch)
		sss.sync[i] = ch
	}

	return nil
}

func (sss *splunkSpanSink) Stop() {
	for _, signal := range sss.sync {
		close(signal)
	}
}

func (sss *splunkSpanSink) Sync() {
	sss.synced.Add(len(sss.sync))
	for _, signal := range sss.sync {
		signal <- struct{}{}
	}
	sss.synced.Wait()
}

func (sss *splunkSpanSink) submitter(sync chan struct{}) {
	for {
		var req *http.Request
		hecReq, err := sss.hec.newRequest()

		ingested := 0
		enc := hecReq.GetEncoder()
	Batch:
		for {
			select {
			case _, ok := <-sync:
				hecReq.Close()
				if !ok {
					// sink is shutting down, exit forever:
					return
				}
				sss.synced.Done()
				break Batch
			case ev := <-sss.ingest:
				ingested++
				if req == nil {
					req, err = hecReq.Start()
					if err != nil {
						sss.log.WithError(err).
							Warn("Could not create HEC request")
						time.Sleep(1 * time.Second)
						break Batch
					}
					go sss.makeHTTPRequest(req)
				}
				err = enc.Encode(ev)
				if err != nil {
					sss.log.WithError(err).
						WithField("event", ev).
						Warn("Could not json-encode HEC event")
					continue Batch
				}
				if ingested >= sss.batchSize {
					// we consumed the batch size's worth, let's send it:
					hecReq.Close()
					break Batch
				}
			}
		}
	}
}

func (sss *splunkSpanSink) makeHTTPRequest(req *http.Request) {
	samples := &ssf.Samples{}
	defer metrics.Report(sss.traceClient, samples)
	const successMetric = "splunk.hec_submission_success_total"
	const failureMetric = "splunk.hec_submission_failed_total"
	const timingMetric = "splunk.span_submission_lifetime_ns"
	start := time.Now()
	defer func() {
		samples.Add(ssf.Timing(timingMetric, time.Now().Sub(start),
			time.Nanosecond, map[string]string{}))
	}()

	resp, err := sss.httpClient.Do(req)
	if uerr, ok := err.(*url.Error); ok && uerr.Timeout() {
		// don't report a sentry-able error for timeouts:
		samples.Add(ssf.Count(failureMetric, 1, map[string]string{
			"cause": "submission_timeout",
		}))
		return
	}
	if err != nil {
		samples.Add(ssf.Count(failureMetric, 1, map[string]string{
			"cause": "execution",
		}))
		return
	}

	defer func() {
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	var cause string
	var statusCode int

	switch resp.StatusCode {
	case http.StatusOK:
		// Everything went well - discard the body so the
		// connection stays alive and early-return (the rest
		// of this function is dedicated to error handling):
		samples.Add(ssf.Count(successMetric, 1, map[string]string{}))
		return
	case http.StatusInternalServerError:
		cause = "internal_server_error"
		statusCode = 8
	case http.StatusServiceUnavailable:
		// This status happens when splunk is out of capacity,
		// no need to report a bug or parse the body for it:
		cause = "service_unavailable"
		statusCode = 9
	default:
		// Something else is wrong, let's parse the body and
		// report a detailed error:
		var parsed Response
		dec := json.NewDecoder(resp.Body)
		err := dec.Decode(&parsed)
		if err != nil {
			sss.log.WithError(err).
				WithField("http_status_code", resp.StatusCode).
				Warn("Could not parse response from splunk HEC")
			return
		}
		cause = "error"
		statusCode = parsed.Code
		sss.log.WithFields(logrus.Fields{
			"http_status_code":  resp.StatusCode,
			"hec_status_code":   parsed.Code,
			"hec_response_text": parsed.Text,
			"event_number":      parsed.InvalidEventNumber,
		}).Error("Error response from Splunk HEC")
	}
	samples.Add(ssf.Count(failureMetric, 1, map[string]string{
		"cause":       cause,
		"status_code": strconv.Itoa(statusCode),
	}))
}

// Flush takes the batched-up events and sends them to the HEC
// endpoint for ingestion. If set, it uses the send timeout configured
// for the span batch.
func (sss *splunkSpanSink) Flush() {
	// make the submitters open a new HTTP request:
	sss.Sync()

	// report the sink stats:
	samples := &ssf.Samples{}
	samples.Add(
		ssf.Count(
			sinks.MetricKeyTotalSpansFlushed,
			float32(atomic.SwapUint32(&sss.ingestedSpans, 0)),
			map[string]string{"sink": sss.Name()}),
		ssf.Count(
			sinks.MetricKeyTotalSpansDropped,
			float32(atomic.SwapUint32(&sss.droppedSpans, 0)),
			map[string]string{"sink": sss.Name()},
		),
		ssf.Count(
			sinks.MetricKeyTotalSpansSkipped,
			float32(atomic.SwapUint32(&sss.skippedSpans, 0)),
			map[string]string{"sink": sss.Name()},
		),
	)

	metrics.Report(sss.traceClient, samples)
	return
}

// Ingest takes in a span and batches it up to be sent in the next
// Flush() iteration.
func (sss *splunkSpanSink) Ingest(ssfSpan *ssf.SSFSpan) error {
	// Only send properly filled-out spans to the HEC:
	if err := protocol.ValidateTrace(ssfSpan); err != nil {
		return err
	}

	// choose (1/spanSampleRate) spans for sampling if any spans
	// have the traceID of 0 or are declared indicator spans, they
	// will always be chosen, regardless of the sample rate.
	if !ssfSpan.Indicator && ssfSpan.TraceId%sss.spanSampleRate != 0 {
		atomic.AddUint32(&sss.skippedSpans, 1)
		return nil
	}

	ctx := context.Background()
	if sss.ingestTimeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, sss.ingestTimeout)
		defer cancel()
	}

	serialized := SerializedSSF{
		TraceId:        strconv.FormatInt(ssfSpan.TraceId, 10),
		Id:             strconv.FormatInt(ssfSpan.Id, 10),
		ParentId:       strconv.FormatInt(ssfSpan.ParentId, 10),
		StartTimestamp: float64(ssfSpan.StartTimestamp) / float64(time.Second),
		EndTimestamp:   float64(ssfSpan.EndTimestamp) / float64(time.Second),
		Duration:       ssfSpan.EndTimestamp - ssfSpan.StartTimestamp,
		Error:          ssfSpan.Error,
		Service:        ssfSpan.Service,
		Tags:           ssfSpan.Tags,
		Indicator:      ssfSpan.Indicator,
		Name:           ssfSpan.Name,
	}

	event := &Event{
		Event: serialized,
	}
	event.SetTime(time.Unix(0, ssfSpan.StartTimestamp))
	event.SetHost(sss.hostname)
	event.SetSourceType(ssfSpan.Service)

	event.SetTime(time.Unix(0, ssfSpan.StartTimestamp))
	select {
	case sss.ingest <- event:
		atomic.AddUint32(&sss.ingestedSpans, 1)
	case <-ctx.Done():
		atomic.AddUint32(&sss.droppedSpans, 1)
	}
	return nil
}

// SerializedSSF holds a set of fields in a format that Splunk can
// handle (it can't handle int64s, and we don't want to round our
// traceID to the thousands place).  This is mildly redundant, but oh
// well.
type SerializedSSF struct {
	TraceId        string            `json:"trace_id"`
	Id             string            `json:"id"`
	ParentId       string            `json:"parent_id"`
	StartTimestamp float64           `json:"start_timestamp"`
	EndTimestamp   float64           `json:"end_timestamp"`
	Duration       int64             `json:"duration_ns"`
	Error          bool              `json:"error"`
	Service        string            `json:"service"`
	Tags           map[string]string `json:"tags"`
	Indicator      bool              `json:"indicator"`
	Name           string            `json:"name"`
}
