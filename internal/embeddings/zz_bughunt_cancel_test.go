package embeddings_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/embeddings"
)

// A caller that gives up on its own (its context is cancelled) is not a
// failure of the endpoint: the next caller must still reach the endpoint.
func TestBughuntCallerCancelOpensCircuit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}))
	}))
	defer server.Close()

	client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: "m"})
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = client.Embed(cancelled, []string{"one search the user abandoned"})
	require.Error(t, err)

	_, err = client.Embed(t.Context(), []string{"another user's search"})
	assert.NoError(t, err, "a healthy endpoint is skipped for 30 s because one caller cancelled")
}
