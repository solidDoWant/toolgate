package embeddings_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/embeddings"
)

func TestEmbedBatchesAndOrders(t *testing.T) {
	var sizes []int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer key", r.Header.Get("Authorization"))

		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "m", in.Model)

		sizes = append(sizes, len(in.Input))

		// Answer in reverse order; the client must order by index.
		data := []map[string]any{}
		for index := len(in.Input) - 1; index >= 0; index-- {
			data = append(data, map[string]any{"index": index, "embedding": []float32{float32(len(in.Input[index])), 1}})
		}

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer server.Close()

	client, err := embeddings.New(embeddings.Options{URL: server.URL + "/", Model: "m", APIKey: "key"})
	require.NoError(t, err)

	texts := make([]string, embeddings.BatchSize+1)
	for index := range texts {
		texts[index] = string(make([]byte, index+1))
	}

	vectors, err := client.Embed(t.Context(), texts)
	require.NoError(t, err)
	require.Len(t, vectors, len(texts))
	assert.Equal(t, []int{embeddings.BatchSize, 1}, sizes)

	for index, vector := range vectors {
		assert.Equal(t, float32(index+1), vector[0])
	}
}

func TestEmbedFailures(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"not JSON": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("nope"))
		},
		"wrong count": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		},
		"slow": func(_ http.ResponseWriter, _ *http.Request) { time.Sleep(time.Second) },
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()

			client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m", Timeout: 100 * time.Millisecond})
			require.NoError(t, err)

			_, err = client.Embed(t.Context(), []string{"secret query"})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret query")
		})
	}
}

func TestNewValidates(t *testing.T) {
	_, err := embeddings.New(embeddings.Options{URL: "ftp://x", Model: "m"})
	require.Error(t, err)

	_, err = embeddings.New(embeddings.Options{URL: "http://x"})
	require.Error(t, err)
}

func TestFormatVector(t *testing.T) {
	assert.Equal(t, "[0.5,1,-2.25]", embeddings.FormatVector([]float32{0.5, 1, -2.25}))
	assert.Equal(t, "", embeddings.FormatVector(nil))
}

func TestEmbedRecordsRequestDuration(t *testing.T) {
	const delay = 30 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}))
	}))
	defer server.Close()

	metrics := embeddings.NewMetrics()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.Collectors()...)

	client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m"}, metrics)
	require.NoError(t, err)

	_, err = client.EmbedFor(t.Context(), embeddings.OperationQuery, []string{"a"})
	require.NoError(t, err)
	_, err = client.EmbedFor(t.Context(), embeddings.OperationDocument, []string{"a"})
	require.NoError(t, err)
	_, err = client.EmbedFor(t.Context(), embeddings.OperationDocument, []string{"b"})
	require.NoError(t, err)

	families, err := registry.Gather()
	require.NoError(t, err)

	counts := map[string]uint64{}

	for _, family := range families {
		if family.GetName() != "toolgate_embeddings_request_duration_seconds" {
			continue
		}

		for _, metric := range family.GetMetric() {
			operation := metric.GetLabel()[0].GetValue()
			counts[operation] = metric.GetHistogram().GetSampleCount()

			assert.GreaterOrEqual(t, metric.GetHistogram().GetSampleSum(), delay.Seconds()*float64(counts[operation]))
			assert.Len(t, metric.GetHistogram().GetBucket(), len(embeddings.DurationBuckets))
		}
	}

	assert.Equal(t, map[string]uint64{"query": 1, "document": 2}, counts)
}

func TestEmbedCallerGivingUpKeepsCircuitClosed(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}))
	}))
	defer server.Close()

	client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m"})
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = client.Embed(cancelled, []string{"abandoned"})
	require.Error(t, err)

	_, err = client.Embed(t.Context(), []string{"next"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load())
}

func TestEmbedEndpointTimeoutOpensCircuit(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))

	defer server.Close()
	defer close(release)

	client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m", Timeout: 50 * time.Millisecond})
	require.NoError(t, err)

	_, err = client.Embed(t.Context(), []string{"a"})
	require.Error(t, err)

	_, err = client.Embed(t.Context(), []string{"a"})
	require.ErrorIs(t, err, embeddings.ErrCircuitOpen)
}

// A scaled-down server must hear about demand even while the circuit is
// open, or it would never be started again.
func TestEmbedReportsDemandEvenWithCircuitOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	var demands atomic.Int32

	client, err := embeddings.New(embeddings.Options{
		URL: server.URL, Model: "m",
		OnDemand: func(context.Context) { demands.Add(1) },
	})
	require.NoError(t, err)

	_, err = client.Embed(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, int32(0), demands.Load(), "nothing to embed is no demand")

	_, err = client.Embed(t.Context(), []string{"a"})
	require.Error(t, err)
	assert.Equal(t, int32(1), demands.Load())

	_, err = client.EmbedFor(t.Context(), embeddings.OperationDocument, []string{"a"})
	require.ErrorIs(t, err, embeddings.ErrCircuitOpen)
	assert.Equal(t, int32(2), demands.Load())
}
