package embeddings_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestCosine(t *testing.T) {
	assert.InDelta(t, 1, embeddings.Cosine([]float32{1, 2}, []float32{2, 4}), 1e-9)
	assert.InDelta(t, 0, embeddings.Cosine([]float32{1, 0}, []float32{0, 1}), 1e-9)
	assert.Zero(t, embeddings.Cosine([]float32{1}, []float32{1, 2}))
	assert.Zero(t, embeddings.Cosine(nil, nil))
	assert.Zero(t, embeddings.Cosine([]float32{0, 0}, []float32{1, 1}))
}
