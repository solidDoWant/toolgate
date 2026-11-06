package embeddings_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/embeddings"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestEmbedCircuit(t *testing.T) {
	var (
		calls   atomic.Int32
		healthy atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}))
	}))
	defer server.Close()

	clock := &fakeClock{now: time.Unix(1000, 0)}
	metrics := embeddings.NewMetrics()
	client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m", Now: clock.Now}, metrics)
	require.NoError(t, err)

	// A failure opens the circuit: the next call is skipped, not sent.
	_, err = client.Embed(t.Context(), []string{"a"})
	require.Error(t, err)
	assert.EqualValues(t, 1, calls.Load())

	_, err = client.Embed(t.Context(), []string{"a"})
	require.ErrorIs(t, err, embeddings.ErrCircuitOpen)
	assert.EqualValues(t, 1, calls.Load())

	client.CountError("query", err)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.Counter("query", "circuit_open")))

	// Just before the end of the 30 seconds it is still open; at 30 seconds it
	// tries again, and a success closes it.
	healthy.Store(true)

	clock.now = clock.now.Add(embeddings.CircuitOpenFor - time.Nanosecond)

	_, err = client.Embed(t.Context(), []string{"a"})
	require.ErrorIs(t, err, embeddings.ErrCircuitOpen)

	clock.now = clock.now.Add(time.Nanosecond)

	_, err = client.Embed(t.Context(), []string{"a"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())

	_, err = client.Embed(t.Context(), []string{"a"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load())
}

func TestRerank(t *testing.T) {
	var (
		calls   atomic.Int32
		healthy atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/rerank", r.URL.Path)

		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		var in struct {
			Query string   `json:"query"`
			Texts []string `json:"texts"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "q", in.Query)

		// Scores favour the longest text; the answer is in index order.
		out := make([]map[string]any, len(in.Texts))
		for index, text := range in.Texts {
			out[index] = map[string]any{"index": index, "score": float64(len(text))}
		}

		assert.NoError(t, json.NewEncoder(w).Encode(out))
	}))
	defer server.Close()

	clock := &fakeClock{now: time.Unix(1000, 0)}
	reranker, err := embeddings.NewReranker(embeddings.Options{URL: server.URL + "/", Model: "r", Now: clock.Now}, embeddings.NewMetrics())
	require.NoError(t, err)

	_, err = reranker.Rerank(t.Context(), "q", []string{"a"})
	require.Error(t, err)

	_, err = reranker.Rerank(t.Context(), "q", []string{"a"})
	require.ErrorIs(t, err, embeddings.ErrCircuitOpen)
	assert.EqualValues(t, 1, calls.Load())

	healthy.Store(true)

	clock.now = clock.now.Add(embeddings.CircuitOpenFor)

	order, err := reranker.Rerank(t.Context(), "q", []string{"aa", "a", "aaa"})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 0, 1}, order)

	_, err = reranker.Rerank(t.Context(), "q", nil)
	require.Error(t, err)
}
