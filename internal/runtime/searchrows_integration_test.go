//go:build integration

package runtime_test

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apitypes "k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func searchTemplate(uid string, generation int64) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{}
	template.Namespace = "catalog"
	template.Name = "github"
	template.UID = apitypes.UID(uid)
	template.Generation = generation
	template.Spec.Description = "Code hosting"

	return template
}

func searchNames(t *testing.T, queries *db.Queries, uid string) []string {
	t.Helper()

	rows, err := queries.ListToolSearchForTemplate(t.Context(), uid)
	require.NoError(t, err)

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.UpstreamName)
	}

	return names
}

// Row 189: two ready writes of one template at once.
func TestConcurrentReadyWritesRebuildSearchRows(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	template := searchTemplate("uid-1", 1)
	startStore := runtime.PostgresStartStore{
		Pool: pool,
		RunningTemplate: func(instance db.Instance) (*v1alpha1.MCPServerTemplate, bool) {
			return template, true
		},
	}

	for round := range 30 {
		first := fmt.Sprintf("%032d", round*2)
		second := fmt.Sprintf("%032d", round*2+1)

		// Two generations, so the catalog upserts touch different rows and
		// only the rebuild lock orders the rebuilds.
		for index, id := range []string{first, second} {
			generation := int64(index + 1)
			insertInstance(t, queries, id, "uid-1", generation)
			changed, err := startStore.MarkStarting(t.Context(), id, generation, nil, time.Time{})
			require.NoError(t, err)
			require.True(t, changed)
		}

		var wait sync.WaitGroup

		errs := make([]error, 2)

		for index, id := range []string{first, second} {
			wait.Add(1)

			go func() {
				defer wait.Done()

				_, errs[index] = startStore.MarkReady(t.Context(), id, []byte(`[{"name":"create_issue"},{"name":"list_repos"}]`))
			}()
		}

		wait.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		assert.Equal(t, []string{"create_issue", "list_repos"}, searchNames(t, queries, "uid-1"))
	}
}

func TestRebuildToolSearchRows(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx := t.Context()

	go func() { _ = listener.Run(ctx) }()

	events, unsubscribe := listener.Subscribe()
	defer unsubscribe()

	require.NoError(t, queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
		TemplateUid: "uid-1", Generation: 1, TemplateNs: "catalog", TemplateName: "github", InstanceID: "i",
		Tools: []byte(`[{"name":"createIssue","description":"Open an issue","inputSchema":{"type":"object","properties":{"title":{"description":"Heading"}}}},{"name":"secret_tool"}]`),
	}))

	template := searchTemplate("uid-1", 2)
	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"secret_tool"}}

	current := map[string]*v1alpha1.MCPServerTemplate{"uid-1": template}
	rebuilder := &runtime.SearchRebuilder{Pool: pool, Templates: func() map[string]*v1alpha1.MCPServerTemplate { return current }}

	// Stale rows of a template nobody knows are removed by the first sync.
	require.NoError(t, queries.InsertToolSearch(ctx, db.InsertToolSearchParams{
		TemplateUid: "uid-old", TemplateNs: "catalog", TemplateName: "old", SourceGeneration: 1, UpstreamName: "x", ExposedName: "x",
	}))

	require.NoError(t, rebuilder.Sync(ctx))

	// Hidden tools are never in the rows; an older generation's list counts.
	assert.Equal(t, []string{"createIssue"}, searchNames(t, queries, "uid-1"))
	assert.Empty(t, searchNames(t, queries, "uid-old"))

	rows, err := queries.ListToolSearchForTemplate(ctx, "uid-1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows[0].SourceGeneration)

	// The four parts carry the weights A to D.
	var weights string

	require.NoError(t, pool.QueryRow(ctx, `SELECT document::text FROM tool_search WHERE template_uid = 'uid-1'`).Scan(&weights))
	// A is the name, B the description, C the server name and its description;
	// D, the default weight, shows with no letter.
	assert.Equal(t, "'code':7C 'creat':1A 'github':6C 'head':10 'host':8C 'issu':2A,5B 'open':3B 'titl':9", weights)

	var matches int

	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM tool_search WHERE document @@ to_tsquery('english', 'creating | heading')`).Scan(&matches))
	assert.Equal(t, 1, matches)

	assertNotified(t, events, "uid-1")

	// A change of curation rebuilds the rows.
	template = searchTemplate("uid-1", 2)
	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"secret_tool"}}
	current = map[string]*v1alpha1.MCPServerTemplate{"uid-1": template}

	require.NoError(t, rebuilder.Sync(ctx))
	assert.Equal(t, []string{"secret_tool"}, searchNames(t, queries, "uid-1"))

	// A template with no valid generation has its rows deleted.
	current = map[string]*v1alpha1.MCPServerTemplate{}

	require.NoError(t, rebuilder.Sync(ctx))
	assert.Empty(t, searchNames(t, queries, "uid-1"))
}

func assertNotified(t *testing.T, events <-chan store.Event, uid string) {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case event := <-events:
			if event.Kind == store.KindToolSearch && event.ID == uid {
				return
			}
		case <-deadline:
			require.Fail(t, "no toolsearch notification")
		}
	}
}

// Row 190: a new leader rebuilds every template's rows.
func TestNewLeaderRebuildsAllSearchRows(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	ctx := t.Context()

	for _, uid := range []string{"uid-1", "uid-2"} {
		require.NoError(t, queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
			TemplateUid: uid, Generation: 1, TemplateNs: "catalog", TemplateName: "github", InstanceID: "i", Tools: []byte(`[{"name":"one"}]`),
		}))
	}

	templates := map[string]*v1alpha1.MCPServerTemplate{"uid-1": searchTemplate("uid-1", 1), "uid-2": searchTemplate("uid-2", 1)}

	// Rows that an earlier leader left in an old state.
	require.NoError(t, queries.InsertToolSearch(ctx, db.InsertToolSearchParams{
		TemplateUid: "uid-1", TemplateNs: "catalog", TemplateName: "github", SourceGeneration: 1, UpstreamName: "stale", ExposedName: "stale",
	}))

	rebuilder := &runtime.SearchRebuilder{Pool: pool, Templates: func() map[string]*v1alpha1.MCPServerTemplate { return templates }}
	require.NoError(t, rebuilder.Sync(ctx))

	assert.Equal(t, []string{"one"}, searchNames(t, queries, "uid-1"))
	assert.Equal(t, []string{"one"}, searchNames(t, queries, "uid-2"))

	// The cleanup pass deletes rows of templates that no longer exist.
	cleaner := &runtime.CatalogCleaner{Queries: queries, TemplateUIDs: func() map[string]bool { return map[string]bool{"uid-1": true} }}
	require.NoError(t, cleaner.Run(ctx))
	assert.Equal(t, []string{"one"}, searchNames(t, queries, "uid-1"))
	assert.Empty(t, searchNames(t, queries, "uid-2"))
}

func TestSearchRebuilderWritesOnlyWhileElected(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	require.NoError(t, queries.UpsertToolCatalog(t.Context(), db.UpsertToolCatalogParams{
		TemplateUid: "uid-1", Generation: 1, TemplateNs: "catalog", TemplateName: "github", InstanceID: "i", Tools: []byte(`[{"name":"one"}]`),
	}))

	rebuilder := &runtime.SearchRebuilder{
		Pool: pool, Fence: noLease{},
		Templates: func() map[string]*v1alpha1.MCPServerTemplate {
			return map[string]*v1alpha1.MCPServerTemplate{"uid-1": searchTemplate("uid-1", 1)}
		},
	}

	require.ErrorIs(t, rebuilder.Sync(t.Context()), runtime.ErrNotLeader)
	assert.Empty(t, searchNames(t, queries, "uid-1"))
}

type noLease struct{}

func (noLease) HoldsLease() bool { return false }
