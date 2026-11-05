package testutil

import (
	"compress/gzip"
	"io"
	"net/http"
	"sync"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// maxOTLPRequestBytes bounds one export request the receiver reads.
const maxOTLPRequestBytes = 16 << 20

// OTLPReceiver is an http.Handler that collects the spans exported to it
// over OTLP/HTTP (protobuf) at /v1/traces.
type OTLPReceiver struct {
	mutex sync.Mutex
	spans []*tracepb.Span
}

func (r *OTLPReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/traces" {
		http.NotFound(writer, request)

		return
	}

	var reader io.Reader = http.MaxBytesReader(writer, request.Body, maxOTLPRequestBytes)

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

	response, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})

	writer.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = writer.Write(response)
}

// Spans returns a copy of the spans received so far.
func (r *OTLPReceiver) Spans() []*tracepb.Span {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]*tracepb.Span(nil), r.spans...)
}
