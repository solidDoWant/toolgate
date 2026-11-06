//go:build integration

package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// An endpoint whose vectors change length under the same model name (the
// model behind it was swapped, as text-embeddings-inference ignores the model
// field) must not fail the search: the search falls back to words.
func TestFindToolsVectorDimensionMismatch(t *testing.T) {
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

	metrics := embeddings.NewMetrics()
	embedder, err := embeddings.New(embeddings.Options{URL: endpoint.URL, Model: "m"}, metrics)
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
	assert.Equal(t, degradedNote, found.Note)
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.Counter("query", "dimension_mismatch")), 0)

	// The leader's next sync re-embeds the rows with the new length.
	dimensions.Store(2)

	rebuilder := &runtime.SearchRebuilder{Pool: fixture.pool, Templates: fixture.catalog.LastValidByUID, Embedder: embedder}
	require.NoError(t, rebuilder.Sync(t.Context()))
	dimensions.Store(3)
	require.NoError(t, rebuilder.Sync(t.Context()))

	found, result = findTools(t, session, "create issue", 0)
	assert.False(t, result.IsError, "the search failed: %s", resultText(result))
	assert.Equal(t, []string{"srv__create_issue"}, foundNames(found))
	assert.Empty(t, found.Note)
}
