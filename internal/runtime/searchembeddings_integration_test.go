//go:build integration

package runtime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// fakeEmbeddings answers every text with the vector [1, 2, 3] while healthy,
// and counts its calls.
func fakeEmbeddings(t *testing.T, healthy *atomic.Bool, calls *atomic.Int32) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		var in struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&in))

		data := make([]map[string]any, len(in.Input))
		for index := range in.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 2, 3}}
		}

		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)

	return server
}

func embeddingState(t *testing.T, pool *pgxpool.Pool, uid string) (model string, vectorLength int) {
	t.Helper()

	var length int

	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT embedding_model, COALESCE(vector_dims(embedding), 0) FROM tool_search WHERE template_uid = $1`, uid).Scan(&model, &length))

	return model, length
}

// TestSearchRowsEmbeddings covers the embeddings rows of the design: no
// setting makes no call, an endpoint that fails leaves an empty vector and is
// retried, and a changed model embeds the rows again.
func TestSearchRowsEmbeddings(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	ctx := t.Context()

	require.NoError(t, queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
		TemplateUid: "uid-1", Generation: 1, TemplateNs: "catalog", TemplateName: "github", InstanceID: "i",
		Tools: []byte(`[{"name":"createIssue","description":"Open an issue"}]`),
	}))

	current := map[string]*v1alpha1.MCPServerTemplate{"uid-1": searchTemplate("uid-1", 2)}
	templates := func() map[string]*v1alpha1.MCPServerTemplate { return current }

	var (
		healthy atomic.Bool
		calls   atomic.Int32
	)

	server := fakeEmbeddings(t, &healthy, &calls)

	// The clock lets the test move past the circuit that a failed call opens.
	var offset atomic.Int64

	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	newClient := func(model string) *embeddings.Client {
		client, err := embeddings.New(embeddings.Options{URL: server.URL, Model: model, Now: now})
		require.NoError(t, err)

		return client
	}

	// No setting: no call, no vector.
	plain := &runtime.SearchRebuilder{Pool: pool, Templates: templates}
	require.NoError(t, plain.Sync(ctx))
	assert.Zero(t, calls.Load())

	model, length := embeddingState(t, pool, "uid-1")
	assert.Empty(t, model)
	assert.Zero(t, length)

	// The endpoint is down: the sync succeeds, the row has no vector.
	failing := &runtime.SearchRebuilder{Pool: pool, Templates: templates, Embedder: newClient("model-a")}
	require.NoError(t, failing.Sync(ctx))

	model, length = embeddingState(t, pool, "uid-1")
	assert.Empty(t, model)
	assert.Zero(t, length)

	// The endpoint is back: the next sync embeds the row without a template change.
	healthy.Store(true)
	offset.Store(int64(2 * embeddings.CircuitOpenFor))
	require.NoError(t, failing.Sync(ctx))

	model, length = embeddingState(t, pool, "uid-1")
	assert.Equal(t, "model-a", model)
	assert.Equal(t, 3, length)

	// The model changes: a new leader embeds the row again.
	changed := &runtime.SearchRebuilder{Pool: pool, Templates: templates, Embedder: newClient("model-b")}
	require.NoError(t, changed.Sync(ctx))

	model, _ = embeddingState(t, pool, "uid-1")
	assert.Equal(t, "model-b", model)

	// A row written with another model (by an older leader) is embedded
	// again on the next sync, although the template did not change.
	_, err = pool.Exec(ctx, `UPDATE tool_search SET embedding_model = 'foreign' WHERE template_uid = 'uid-1'`)
	require.NoError(t, err)
	require.NoError(t, changed.Sync(ctx))

	model, _ = embeddingState(t, pool, "uid-1")
	assert.Equal(t, "model-b", model)
}
