//go:build integration

package gateway_test

import (
	"context"
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

// While the leader publishes the embeddings server as down or starting, a
// search ranks by words only without calling the endpoint, counts the skip as
// scaled_down, records demand, and leaves the circuit closed.
func TestFindToolsWhileEmbeddingsServerScaledDown(t *testing.T) {
	var requests atomic.Int32

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		var in struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&in))

		data := make([]map[string]any, len(in.Input))
		for index := range in.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 0}}
		}

		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(endpoint.Close)

	metrics := embeddings.NewMetrics()

	var fixture *mcpFixture

	demand := make(chan struct{}, 8)
	embedder, err := embeddings.New(embeddings.Options{
		URL: endpoint.URL, Model: "m", OnDemand: func(ctx context.Context) { demand <- struct{}{} },
		ScaledDown: func(ctx context.Context) bool { return runtime.EmbeddingsServerScaledDown(fixture.pool)(ctx) },
	}, metrics)
	require.NoError(t, err)

	fixture = newMCPFixture(t, func(options *gateway.MCPOptions) { options.Embedder = embedder })
	fixture.embedder = embedder
	template := fixture.addEnableTemplate("srv", "per-user", anyone)

	_, err = fixture.pool.Exec(t.Context(), "UPDATE embeddings_server SET state = 'ready'")
	require.NoError(t, err)
	fixture.declareTools(t, template, declared("create_issue", "Create an issue"))

	for _, state := range []string{"down", "starting"} {
		_, err = fixture.pool.Exec(t.Context(), "UPDATE embeddings_server SET state = $1", state)
		require.NoError(t, err)

		requests.Store(0)

		before := testutil.ToFloat64(metrics.Counter("query", "scaled_down"))

		session := fixture.connect(t, searching(alice()))
		found, result := findTools(t, session, "create issue", 0)

		assert.False(t, result.IsError, "the search failed: %s", resultText(result))
		assert.Equal(t, []string{"srv__create_issue"}, foundNames(found), state)
		assert.Equal(t, degradedNote, found.Note, state)
		assert.Zero(t, requests.Load(), state)
		assert.InDelta(t, before+1, testutil.ToFloat64(metrics.Counter("query", "scaled_down")), 0, state)
		assert.Zero(t, testutil.ToFloat64(metrics.Counter("query", "error")), state)
		assert.NotEmpty(t, demand, state)
	}

	_, err = fixture.pool.Exec(t.Context(), "UPDATE embeddings_server SET state = 'ready'")
	require.NoError(t, err)

	session := fixture.connect(t, searching(alice()))
	found, _ := findTools(t, session, "create issue", 0)

	assert.Empty(t, found.Note, "the circuit stayed closed")
	assert.NotZero(t, requests.Load())
}
