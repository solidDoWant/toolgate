//go:build integration

package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

// An endpoint whose vectors change length under the same model name (the
// model behind it was swapped, as text-embeddings-inference ignores the model
// field) must not fail the search: the search falls back to words.
func TestBughuntSearchVectorDimensionMismatch(t *testing.T) {
	var dimensions atomic.Int32

	dimensions.Store(2)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&in))

		data := make([]map[string]any, len(in.Input))
		for index := range in.Input {
			vector := make([]float32, dimensions.Load())
			vector[0] = 1
			data[index] = map[string]any{"index": index, "embedding": vector}
		}

		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(endpoint.Close)

	embedder, err := embeddings.New(embeddings.Options{URL: endpoint.URL, Model: "m"})
	require.NoError(t, err)

	fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.Embedder = embedder })
	fixture.embedder = embedder
	template := fixture.addEnableTemplate("srv", "per-user", anyone)
	fixture.declareTools(t, template, declared("create_issue", "Create an issue"))

	// The endpoint now answers with 3-dimensional vectors.
	dimensions.Store(3)

	session := fixture.connect(t, searching(alice()))
	found, result := findTools(t, session, "create issue", 0)

	assert.False(t, result.IsError, "the search failed: %s", resultText(result))
	assert.Equal(t, []string{"srv__create_issue"}, foundNames(found))
}
