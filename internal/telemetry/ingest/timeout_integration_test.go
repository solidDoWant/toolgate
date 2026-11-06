//go:build integration

package ingest_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
)

// A backend that never answers gives 503 with Retry-After after at most 10 s.
func TestIngestSlowBackendGivesUnavailableWithinTenSeconds(t *testing.T) {
	fake := newBackend(t)
	hang := make(chan struct{})

	t.Cleanup(func() { close(hang) })

	fake.setAnswer(func(http.ResponseWriter, *http.Request) { <-hang })

	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})
	started := time.Now()

	response, _ := gateway.post(t, post{
		signal: "traces", body: encode(t, ptraceotlp.NewExportRequestFromTraces(sampleTraces()), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	elapsed := time.Since(started)

	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, "5", response.Header.Get("Retry-After"))
	assert.LessOrEqual(t, elapsed, 11*time.Second, "the forward was not bounded by 10 s")
}
