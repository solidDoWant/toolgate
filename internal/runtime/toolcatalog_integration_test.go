//go:build integration

package runtime_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func catalogPool(t *testing.T) (*db.Queries, runtime.PostgresStartStore) {
	t.Helper()

	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	return db.New(pool), runtime.PostgresStartStore{Pool: pool}
}

func insertInstance(t *testing.T, queries *db.Queries, id, uid string, generation int64) {
	t.Helper()

	_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: "catalog", TemplateName: "github", TemplateUid: uid,
		Generation: generation, Scope: "per-user", ScopeKey: "user:" + id, Namespace: "tg-" + id,
	})
	require.NoError(t, err)
}

func TestStartStoreLearnsTools(t *testing.T) {
	queries, startStore := catalogPool(t)
	insertInstance(t, queries, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "uid-1", 3)

	changed, err := startStore.MarkStarting(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 3, nil, time.Time{}, nil)
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = startStore.MarkReady(t.Context(), db.Instance{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, []byte(`[{"name":"one"}]`), runtime.RevisionStateful)
	require.NoError(t, err)
	require.True(t, changed)

	row, err := queries.GetToolCatalog(t.Context(), db.GetToolCatalogParams{TemplateUid: "uid-1", Generation: 3})
	require.NoError(t, err)
	assert.JSONEq(t, `[{"name":"one"}]`, string(row.Tools))
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", row.InstanceID)
	assert.Equal(t, "github", row.TemplateName)
	assert.True(t, row.LearnedAt.Valid)

	// A change to a ready instance's list is learned too.
	changed, err = startStore.SetTools(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte(`[{"name":"two"}]`))
	require.NoError(t, err)
	require.True(t, changed)

	row, err = queries.GetToolCatalog(t.Context(), db.GetToolCatalogParams{TemplateUid: "uid-1", Generation: 3})
	require.NoError(t, err)
	assert.JSONEq(t, `[{"name":"two"}]`, string(row.Tools))

	// A write that changes nothing learns nothing.
	insertInstance(t, queries, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "uid-1", 4)
	changed, err = startStore.MarkReady(t.Context(), db.Instance{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, []byte(`[{"name":"x"}]`), runtime.RevisionStateful)
	require.NoError(t, err)
	assert.False(t, changed, "inst-b is still pending")

	rows, err := queries.ListToolCatalogForTemplate(t.Context(), "uid-1")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestStartStoreLearnsNothingForDeclaredTools(t *testing.T) {
	queries, startStore := catalogPool(t)
	startStore.DeclaresTools = func(instance db.Instance) bool { return instance.TemplateUid == "uid-declared" }

	for id, uid := range map[string]string{"dddddddddddddddddddddddddddddddd": "uid-declared", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee": "uid-learned"} {
		insertInstance(t, queries, id, uid, 1)

		changed, err := startStore.MarkStarting(t.Context(), id, 1, nil, time.Time{}, nil)
		require.NoError(t, err)
		require.True(t, changed)

		changed, err = startStore.MarkReady(t.Context(), db.Instance{ID: id}, []byte(`[]`), runtime.RevisionStateful)
		require.NoError(t, err)
		require.True(t, changed)
	}

	declared, err := queries.ListToolCatalogForTemplate(t.Context(), "uid-declared")
	require.NoError(t, err)
	assert.Empty(t, declared)

	learned, err := queries.ListToolCatalogForTemplate(t.Context(), "uid-learned")
	require.NoError(t, err)
	assert.Len(t, learned, 1)
}

func TestStartStoreToolCatalogSizeLimit(t *testing.T) {
	queries, startStore := catalogPool(t)

	// jsonb's text form adds a space after each colon and comma, so a list
	// that is under the limit as compact JSON can be over it when stored.
	// The instance still becomes ready; the list is not learned.
	big := `[` + strings.Repeat(`{"a":1},`, 130000) + `{"a":1}]`
	require.LessOrEqual(t, len(big), 1<<20)

	insertInstance(t, queries, "cccccccccccccccccccccccccccccccc", "uid-big", 1)

	changed, err := startStore.MarkStarting(t.Context(), "cccccccccccccccccccccccccccccccc", 1, nil, time.Time{}, nil)
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = startStore.MarkReady(t.Context(), db.Instance{ID: "cccccccccccccccccccccccccccccccc"}, []byte(big), runtime.RevisionStateful)
	require.NoError(t, err)
	assert.True(t, changed)

	rows, err := queries.ListToolCatalogForTemplate(t.Context(), "uid-big")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCatalogCleaner(t *testing.T) {
	queries, _ := catalogPool(t)

	for _, row := range []struct {
		uid        string
		generation int64
	}{{"gone", 1}, {"kept", 1}, {"kept", 2}, {"kept", 3}, {"kept", 5}, {"single", 1}} {
		require.NoError(t, queries.UpsertToolCatalog(t.Context(), db.UpsertToolCatalogParams{
			TemplateUid: row.uid, Generation: row.generation, TemplateNs: "catalog", TemplateName: "github",
			Tools: []byte(`[]`), InstanceID: "i",
		}))
	}

	cleaner := &runtime.CatalogCleaner{Queries: queries, TemplateUIDs: func() map[string]bool {
		return map[string]bool{"kept": true, "single": true}
	}}
	require.NoError(t, cleaner.Run(t.Context()))

	remaining := map[string][]int64{}

	for _, uid := range []string{"gone", "kept", "single"} {
		rows, err := queries.ListToolCatalogForTemplate(t.Context(), uid)
		require.NoError(t, err)

		for _, row := range rows {
			remaining[uid] = append(remaining[uid], row.Generation)
		}
	}

	assert.Equal(t, map[string][]int64{"kept": {5, 3}, "single": {1}}, remaining)
}
