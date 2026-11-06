package testutil

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// maxOTLPRequestBytes bounds one export request the receiver reads.
const maxOTLPRequestBytes = 16 << 20

// OTLPReceiver is an http.Handler that collects the spans exported to it
// over OTLP/HTTP (protobuf) at /v1/traces.
//
// When ForwardTo is set, every export (all three signals) is also sent on
// to that OTLP/HTTP base URL, and the receiver answers with the backend's
// answer. Without it, only traces are accepted.
type OTLPReceiver struct {
	ForwardTo string

	mutex sync.Mutex
	spans []*tracepb.Span
}

func (r *OTLPReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/traces" && (r.ForwardTo == "" || !strings.HasPrefix(request.URL.Path, "/v1/")) {
		http.NotFound(writer, request)

		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxOTLPRequestBytes))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	if request.URL.Path != "/v1/traces" {
		r.forward(writer, request, raw)

		return
	}

	var reader io.Reader = bytes.NewReader(raw)

	if request.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		reader = io.LimitReader(gzipReader, maxOTLPRequestBytes)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	var export collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(data, &export); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	r.mutex.Lock()
	for _, resourceSpans := range export.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			r.spans = append(r.spans, scopeSpans.GetSpans()...)
		}
	}
	r.mutex.Unlock()

	if r.ForwardTo != "" {
		r.forward(writer, request, raw)

		return
	}

	response, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})

	writer.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = writer.Write(response)
}

// forward sends the raw export on to ForwardTo and relays the answer.
func (r *OTLPReceiver) forward(writer http.ResponseWriter, request *http.Request, raw []byte) {
	forwarded, err := http.NewRequestWithContext(request.Context(), http.MethodPost, r.ForwardTo+request.URL.Path, bytes.NewReader(raw))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)

		return
	}

	for _, name := range []string{"Content-Type", "Content-Encoding"} {
		if value := request.Header.Get(name); value != "" {
			forwarded.Header.Set(name, value)
		}
	}

	response, err := http.DefaultClient.Do(forwarded)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)

		return
	}

	defer func() { _ = response.Body.Close() }()

	writer.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, io.LimitReader(response.Body, maxOTLPRequestBytes))
}

// Spans returns a copy of the spans received so far.
func (r *OTLPReceiver) Spans() []*tracepb.Span {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]*tracepb.Span(nil), r.spans...)
}
