//go:build integration

package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/resultcache"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// recordingStore wraps a Store, counts its Puts, and can fail its Gets and
// Puts.
type recordingStore struct {
	resultcache.Store

	mu      sync.Mutex
	puts    int
	failGet bool
	failPut bool
}

func (s *recordingStore) Get(ctx context.Context, key resultcache.Key) (resultcache.Hit, bool, error) {
	s.mu.Lock()
	fail := s.failGet
	s.mu.Unlock()

	if fail {
		return resultcache.Hit{}, false, errors.New("lookup broke")
	}

	return s.Store.Get(ctx, key)
}

func (s *recordingStore) Put(ctx context.Context, key resultcache.Key, result []byte, ttl time.Duration) (resultcache.PutResult, error) {
	s.mu.Lock()
	s.puts++
	fail := s.failPut
	s.mu.Unlock()

	if fail {
		return "", errors.New("store broke")
	}

	return s.Store.Put(ctx, key, result, ttl)
}

// fail sets whether Get and Put fail.
func (s *recordingStore) fail(get, put bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failGet, s.failPut = get, put
}

func (s *recordingStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.puts
}

// cacheFixture is a gateway with the PostgreSQL result cache, metrics and a
// captured log, and the template "parts" whose tools echo and fails have
// cache settings.
type cacheFixture struct {
	*mcpFixture

	metrics  *runtime.Metrics
	store    *recordingStore
	logs     *lockedBuffer
	template *v1alpha1.MCPServerTemplate
	upstream *upstreamServer
}

func newCacheFixture(t *testing.T, maxEntryBytes int, maxTemplateBytes int64, configure ...func(*gateway.MCPOptions)) *cacheFixture {
	t.Helper()

	cache := &cacheFixture{metrics: runtime.NewMetrics(), store: &recordingStore{}, logs: &lockedBuffer{}}
	cache.mcpFixture = newMCPFixture(t, func(options *gateway.MCPOptions) {
		cache.store.Store = resultcache.NewPostgres(options.Database, maxEntryBytes, maxTemplateBytes)
		options.ResultCache = cache.store
		options.Metrics = cache.metrics
		options.Logger = slog.New(slog.NewJSONHandler(cache.logs, nil))

		for _, change := range configure {
			change(options)
		}
	})

	cache.template = cache.addEnableTemplate("parts", "per-user", anyone)
	cache.template.Spec.Tools = map[string]v1alpha1.ToolOverride{
		"echo":  {Cache: &v1alpha1.ToolCache{}},
		"fails": {Cache: &v1alpha1.ToolCache{}},
		"sized": {Cache: &v1alpha1.ToolCache{}},
		// The upstream has no tool missing, so its calls fail.
		"missing": {Cache: &v1alpha1.ToolCache{}},
	}
	cache.catalog.Set(cache.template, nil)
	cache.upstream = cache.startUpstream(t, "parts")

	return cache
}

// change applies edit to the template and offers the result.
func (c *cacheFixture) change(edit func(template *v1alpha1.MCPServerTemplate)) {
	edit(c.template)
	c.catalog.Set(c.template, nil)
}

// entries counts the stored cache entries.
func (c *cacheFixture) entries(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, c.pool.QueryRow(t.Context(), `SELECT count(*) FROM tool_result_cache`).Scan(&count))

	return count
}

// enable enables parts for client and sets its instance's tool list:
// readOnly names the tools marked readOnlyHint.
func (c *cacheFixture) enable(t *testing.T, client *observedClient, scopeKey string, readOnly ...string) string {
	t.Helper()

	instance := c.enableWithTools(t, client, "parts", scopeKey, "echo", "fails", "sized", "plain", "missing")
	c.setReadOnly(t, instance.ID, readOnly...)

	return instance.ID
}

// setReadOnly rewrites the instance's tool list so that exactly the named
// tools have readOnlyHint.
func (c *cacheFixture) setReadOnly(t *testing.T, instanceID string, readOnly ...string) {
	t.Helper()

	_, err := c.pool.Exec(t.Context(), `UPDATE instances SET tools = $1 WHERE id = $2`, readOnlyTools(t, readOnly...), instanceID)
	require.NoError(t, err)
}

// markReadyReadOnly makes a starting instance ready, as the reconciler
// does, with a tool list whose named tools have readOnlyHint.
func (c *cacheFixture) markReadyReadOnly(t *testing.T, instanceID string, readOnly ...string) {
	t.Helper()

	require.NoError(t, pgx.BeginFunc(t.Context(), c.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		instance, err := queries.GetInstance(t.Context(), instanceID)
		if err != nil {
			return err
		}

		if _, err := queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: instanceID, Generation: instance.Generation}); err != nil {
			return err
		}

		if _, err := queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: instanceID, Tools: readOnlyTools(t, readOnly...)}); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, instanceID)
	}))
}

// readOnlyTools is the tool list of a parts instance whose named tools have
// readOnlyHint.
func readOnlyTools(t *testing.T, readOnly ...string) []byte {
	t.Helper()

	listed := []*mcp.Tool{}

	for _, name := range []string{"echo", "fails", "sized", "plain", "missing"} {
		tool := &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}

		for _, marked := range readOnly {
			if marked == name {
				tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
			}
		}

		listed = append(listed, tool)
	}

	encoded, err := json.Marshal(listed)
	require.NoError(t, err)

	return encoded
}

// cacheResults returns the cache fields of every tool.result row of tool, in
// order.
func (c *cacheFixture) cacheResults(t *testing.T, tool string) []map[string]any {
	t.Helper()

	var fields []map[string]any

	for _, row := range c.auditRows(t, "tool.result") {
		if row.Tool != tool {
			continue
		}

		picked := map[string]any{}

		for _, name := range []string{"cache", "cache_skip"} {
			if value, found := row.Details[name]; found {
				picked[name] = value
			}
		}

		fields = append(fields, picked)
	}

	return fields
}

func cacheAge(result *mcp.CallToolResult) (string, bool) {
	settings, ok := result.Meta[resultcache.MetaKey].(map[string]any)
	if !ok {
		return "", false
	}

	age, ok := settings["age"].(string)

	return age, ok
}

func TestRepeatedCallIsServedFromTheCache(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	instanceID := fixture.enable(t, client, "user:alice", "echo")

	first := callTool(t, client.session, "parts__echo", map[string]any{"text": "10k resistor"})
	require.False(t, first.IsError)
	_, found := cacheAge(first)
	assert.False(t, found, "a forwarded result has no cache age")

	second := callTool(t, client.session, "parts__echo", map[string]any{"text": "10k resistor"})
	require.False(t, second.IsError)
	assert.Equal(t, "10k resistor", resultText(second))

	age, found := cacheAge(second)
	require.True(t, found, "a hit carries its age in _meta")
	assert.Regexp(t, `^\d+s$`, age)

	assert.Equal(t, 1, fixture.upstream.callCount("echo"), "the second call must not reach the server")
	assert.Equal(t, []map[string]any{{"cache": "miss"}, {"cache": "hit"}}, fixture.cacheResults(t, "echo"))

	results := fixture.auditRows(t, "tool.result")
	hit := results[len(results)-1].Details
	assert.Equal(t, "ok", hit["outcome"])
	assert.Equal(t, instanceID, hit["instance"])
	assert.Contains(t, hit, "cache_age_ms")
	assert.Contains(t, hit, "duration_ms")

	// The hit's tool.decision is the normal allow, written before the lookup.
	assert.Len(t, fixture.auditRows(t, "tool.decision"), 2)

	// Other arguments are another key.
	callTool(t, client.session, "parts__echo", map[string]any{"text": "1k resistor"})
	assert.Equal(t, 2, fixture.upstream.callCount("echo"))

	lookups := func(result string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.ResultCacheLookups.WithLabelValues("parts", result))
	}
	calls := func(outcome string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.ToolCalls.WithLabelValues("parts", outcome))
	}

	assert.InDelta(t, 1, lookups(runtime.CacheHit), 0)
	assert.InDelta(t, 2, lookups(runtime.CacheMiss), 0)
	assert.InDelta(t, 1, calls(runtime.CallCached), 0)
	assert.InDelta(t, 2, calls(runtime.CallForwarded), 0)
	assert.InDelta(t, 2, promtestutil.ToFloat64(fixture.metrics.ResultCacheStores.WithLabelValues("stored")), 0)

	var duration dto.Metric
	require.NoError(t, fixture.metrics.ToolCallDuration.Write(&duration))
	assert.EqualValues(t, 2, duration.GetHistogram().GetSampleCount(), "a hit has no duration observation")
}

func TestErrorsAreNotCached(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "fails", "missing")

	// An isError result is not stored.
	for range 2 {
		assert.True(t, callTool(t, client.session, "parts__fails", map[string]any{}).IsError)
	}

	assert.Equal(t, 2, fixture.upstream.callCount("fails"))

	// A failed call is not stored: the server has no such tool.
	for range 2 {
		assert.True(t, callTool(t, client.session, "parts__missing", map[string]any{}).IsError)
	}

	assert.Equal(t, 0, fixture.store.putCount(), "nothing is stored for an error or a failed call")

	miss := map[string]any{"cache": "miss"}
	assert.Equal(t, []map[string]any{miss, miss}, fixture.cacheResults(t, "fails"))
	assert.Equal(t, []map[string]any{miss, miss}, fixture.cacheResults(t, "missing"))
	assert.Equal(t, "failed", fixture.auditRows(t, "tool.result")[3].Details["outcome"])
}

func TestToolWithoutReadOnlyHintIsNotCached(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	instanceID := fixture.enable(t, client, "user:alice")

	for range 2 {
		callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
	}

	assert.Equal(t, 2, fixture.upstream.callCount("echo"))
	assert.Equal(t, 0, fixture.store.putCount())

	// The tool gets the hint: the next call stores, the one after hits.
	fixture.setReadOnly(t, instanceID, "echo")

	for range 2 {
		callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
	}

	assert.Equal(t, 3, fixture.upstream.callCount("echo"))

	// The tool loses the hint: its entry is no longer served.
	fixture.setReadOnly(t, instanceID)
	callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
	assert.Equal(t, 4, fixture.upstream.callCount("echo"))

	skipped := map[string]any{"cache": "skipped", "cache_skip": "not-read-only"}
	assert.Equal(t, []map[string]any{skipped, skipped, {"cache": "miss"}, {"cache": "hit"}, skipped}, fixture.cacheResults(t, "echo"))
}

func TestOutdatedInstanceSkipsTheCache(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "echo")

	callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})

	// The template moves on; the instance still runs the old generation.
	changed := fixture.template.DeepCopy()
	changed.Generation++
	fixture.catalog.Set(changed, nil)

	for range 2 {
		callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
	}

	assert.Equal(t, 3, fixture.upstream.callCount("echo"), "an outdated instance neither reads nor stores")

	skipped := map[string]any{"cache": "skipped", "cache_skip": "outdated-instance"}
	assert.Equal(t, []map[string]any{{"cache": "miss"}, skipped, skipped}, fixture.cacheResults(t, "echo"))
}

func TestCacheFailuresForwardTheCall(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "echo")

	const secret = "argument-that-must-not-be-logged"

	// A failed store returns the result and counts failed.
	fixture.store.fail(false, true)

	result := callTool(t, client.session, "parts__echo", map[string]any{"text": secret})
	require.False(t, result.IsError)
	assert.Equal(t, secret, resultText(result))
	assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.ResultCacheStores.WithLabelValues(runtime.CacheStoreFailed)), 0)

	// A failed lookup is a miss, and the call is forwarded.
	fixture.store.fail(false, false)
	callTool(t, client.session, "parts__echo", map[string]any{"text": secret})
	fixture.store.fail(true, false)

	result = callTool(t, client.session, "parts__echo", map[string]any{"text": secret})
	require.False(t, result.IsError)
	assert.Equal(t, 3, fixture.upstream.callCount("echo"))
	assert.Equal(t, "miss", fixture.cacheResults(t, "echo")[2]["cache"])

	logs := fixture.logs.String()
	assert.Contains(t, logs, "tool result cache store failed")
	assert.Contains(t, logs, "tool result cache lookup failed")
	assert.Contains(t, logs, `"tool":"echo"`)
	assert.NotContains(t, logs, secret)
}

func TestArgumentsWithoutKeyAreForwarded(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "echo")

	for range 2 {
		_, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "parts__echo", Arguments: json.RawMessage(`{"text":"a","text":"b"}`),
		})
		require.NoError(t, err)
	}

	assert.Equal(t, 2, fixture.upstream.callCount("echo"))
	assert.Equal(t, 0, fixture.store.putCount())

	skipped := map[string]any{"cache": "skipped", "cache_skip": "arguments"}
	assert.Equal(t, []map[string]any{skipped, skipped}, fixture.cacheResults(t, "echo"))
}

func TestToolWithoutCacheSettingsHasNoCacheFields(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "plain")

	for range 2 {
		callTool(t, client.session, "parts__plain", nil)
	}

	assert.Equal(t, []map[string]any{{}, {}}, fixture.cacheResults(t, "plain"))
	assert.Equal(t, 0, fixture.store.putCount())
	assert.InDelta(t, 0, promtestutil.ToFloat64(fixture.metrics.ResultCacheLookups.WithLabelValues("parts", runtime.CacheMiss)), 0)
}

func TestPolicyRunsBeforeTheCache(t *testing.T) {
	tests := []struct {
		name   string
		refuse func(t *testing.T, fixture *cacheFixture)
	}{
		{name: "a rule", refuse: func(t *testing.T, fixture *cacheFixture) {
			require.NoError(t, fixture.rules.Set(blockServer("parts")))
		}},
		{name: "a temporary block", refuse: func(t *testing.T, fixture *cacheFixture) {
			fixture.insertBlock(t, v1alpha1.RuleMatch{Servers: []string{"parts"}}, "Parts are frozen.", time.Hour, true)
		}},
		{name: "a blocked tool", refuse: func(_ *testing.T, fixture *cacheFixture) {
			fixture.change(func(template *v1alpha1.MCPServerTemplate) {
				template.Spec.Tools["echo"] = v1alpha1.ToolOverride{Blocked: true, Cache: &v1alpha1.ToolCache{}}
			})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCacheFixture(t, 1<<20, 64<<20)
			client := fixture.connectObserved(t, alice())
			fixture.enable(t, client, "user:alice", "echo")

			callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
			require.Equal(t, 1, fixture.entries(t))

			test.refuse(t, fixture)

			require.Eventually(t, func() bool {
				return callTool(t, client.session, "parts__echo", map[string]any{"text": "x"}).IsError
			}, waitTimeout, 50*time.Millisecond)

			decisions := fixture.auditRows(t, "tool.decision")
			assert.Equal(t, "refuse", decisions[len(decisions)-1].Details["decision"])
			assert.Len(t, fixture.auditRows(t, "tool.result"), 1, "a refused call writes no tool.result")
			assert.InDelta(t, 0, promtestutil.ToFloat64(fixture.metrics.ResultCacheLookups.WithLabelValues("parts", runtime.CacheHit)), 0,
				"a refused call never reads the cache")
		})
	}
}

func TestApprovalRunsBeforeTheCache(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20, withApprovalURL)
	fixture.change(func(template *v1alpha1.MCPServerTemplate) {
		template.Spec.Tools["echo"] = v1alpha1.ToolOverride{Approval: "required", Cache: &v1alpha1.ToolCache{}}
	})

	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "echo")

	arguments := map[string]any{"text": "x"}

	// The first call waits for its approval, then is forwarded and stored.
	assert.True(t, callTool(t, client.session, "parts__echo", arguments).IsError)
	fixture.approve(t, fixture.approvals(t)[0].ID)
	require.False(t, callTool(t, client.session, "parts__echo", arguments).IsError)
	require.Equal(t, 1, fixture.entries(t))

	// The second call waits for its own approval before it can hit.
	pending := callTool(t, client.session, "parts__echo", arguments)
	assert.True(t, pending.IsError, "a cached result must not skip the approval")

	approvals := fixture.approvals(t)
	require.Len(t, approvals, 2)
	assert.Equal(t, "pending", approvals[1].Status)

	fixture.approve(t, approvals[1].ID)

	second := callTool(t, client.session, "parts__echo", arguments)
	require.False(t, second.IsError)

	_, found := cacheAge(second)
	assert.True(t, found)
	assert.Equal(t, 1, fixture.upstream.callCount("echo"))
	assert.Equal(t, []map[string]any{{"cache": "miss"}, {"cache": "hit"}}, fixture.cacheResults(t, "echo"))
}

func TestEntriesStayInTheirScope(t *testing.T) {
	tests := []struct {
		name          string
		templateScope string
		cacheScope    string
		first, second user
		firstKey      string
		secondKey     string
	}{
		{name: "another user on a per-user template", templateScope: "per-user",
			first: alice(), second: bob(), firstKey: "user:alice", secondKey: "user:bob"},
		{name: "another session on a per-session cache", templateScope: "per-user", cacheScope: "per-session",
			first: alice(), second: func() user { other := alice(); other.session = "session-2"; return other }(),
			firstKey: "user:alice", secondKey: "user:alice"},
		{name: "the same session ID from another principal", templateScope: "shared", cacheScope: "per-session",
			first: alice(), second: bob(), firstKey: "shared", secondKey: "shared"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCacheFixture(t, 1<<20, 64<<20)
			fixture.change(func(template *v1alpha1.MCPServerTemplate) {
				template.Spec.Scope = test.templateScope
				template.Spec.Tools["echo"] = v1alpha1.ToolOverride{Cache: &v1alpha1.ToolCache{Scope: test.cacheScope}}
			})

			first := fixture.connectObserved(t, test.first)
			instanceID := fixture.enable(t, first, test.firstKey, "echo")

			for range 2 {
				callTool(t, first.session, "parts__echo", map[string]any{"text": "x"})
			}

			require.Equal(t, 1, fixture.upstream.callCount("echo"), "the first client's second call hits")

			second := fixture.connectObserved(t, test.second)
			if test.secondKey == test.firstKey {
				require.Empty(t, awaitEnable(t, startEnable(t, second.session, "parts")).err)
			} else {
				instanceID = fixture.enable(t, second, test.secondKey, "echo")
			}

			fixture.setReadOnly(t, instanceID, "echo")

			result := callTool(t, second.session, "parts__echo", map[string]any{"text": "x"})
			require.False(t, result.IsError)

			_, found := cacheAge(result)
			assert.False(t, found, "another scope must not get the cached result")
			assert.Equal(t, 2, fixture.upstream.callCount("echo"))
		})
	}
}

func TestSessionEndDeletesItsEntries(t *testing.T) {
	setUp := func(t *testing.T) (*cacheFixture, user) {
		fixture := newCacheFixture(t, 1<<20, 64<<20)
		fixture.change(func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools["echo"] = v1alpha1.ToolOverride{Cache: &v1alpha1.ToolCache{Scope: "per-session"}}
		})

		client := alice()
		client.workspace = "repo-a"
		observed := fixture.connectObserved(t, client)
		fixture.enable(t, observed, "user:alice", "echo")

		callTool(t, observed.session, "parts__echo", map[string]any{"text": "x"})
		require.Equal(t, 1, fixture.entries(t))

		return fixture, client
	}

	t.Run("the end transaction deletes them, and the resumed session misses", func(t *testing.T) {
		fixture, client := setUp(t)

		row := fixture.sessionRow(t, "alice", "session-1")
		require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, io.Discard, row.ID, "idle"))
		assert.Equal(t, 0, fixture.entries(t))

		resumed := fixture.connectObserved(t, client)
		require.Empty(t, awaitEnable(t, startEnable(t, resumed.session, "parts")).err)

		result := callTool(t, resumed.session, "parts__echo", map[string]any{"text": "x"})
		_, found := cacheAge(result)
		assert.False(t, found)
		assert.Equal(t, 2, fixture.upstream.callCount("echo"))
	})

	t.Run("a restart in another workspace finds none", func(t *testing.T) {
		fixture, client := setUp(t)

		// An ended session whose entries the end didn't delete.
		fixture.endSession(t)
		require.Equal(t, 1, fixture.entries(t))

		client.workspace = "repo-b"
		restarted := fixture.connectObserved(t, client)
		assert.Equal(t, 0, fixture.entries(t))

		require.Empty(t, awaitEnable(t, startEnable(t, restarted.session, "parts")).err)

		result := callTool(t, restarted.session, "parts__echo", map[string]any{"text": "x"})
		_, found := cacheAge(result)
		assert.False(t, found)
		assert.Equal(t, 2, fixture.upstream.callCount("echo"))
	})
}

func TestSizeLimits(t *testing.T) {
	fixture := newCacheFixture(t, 1024, 4096)
	client := fixture.connectObserved(t, alice())
	fixture.enable(t, client, "user:alice", "sized")

	stores := func(result string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.ResultCacheStores.WithLabelValues(result))
	}

	// An entry over the entry limit is not stored; the result is returned.
	result := callTool(t, client.session, "parts__sized", map[string]any{"size": 1025})
	require.False(t, result.IsError)
	assert.Len(t, resultText(result), 1025-len(`{"content":[{"type":"text","text":""}]}`))
	assert.InDelta(t, 1, stores(string(resultcache.PutEntryTooLarge)), 0)
	assert.Equal(t, 0, fixture.entries(t))

	// Entries of 1024 bytes fill the 4096-byte template: the next is not
	// stored, and its result is still returned.
	for size := 1021; size <= 1024; size++ {
		require.False(t, callTool(t, client.session, "parts__sized", map[string]any{"size": size}).IsError)
	}

	full := callTool(t, client.session, "parts__sized", map[string]any{"size": 1000})
	require.False(t, full.IsError)
	assert.InDelta(t, 4, stores(string(resultcache.PutStored)), 0)
	assert.InDelta(t, 1, stores(string(resultcache.PutTemplateFull)), 0)
	assert.Equal(t, 4, fixture.entries(t))
}

func TestTemplateChangeInvalidatesEntries(t *testing.T) {
	t.Run("a new generation doesn't read the old generation's entries", func(t *testing.T) {
		fixture := newCacheFixture(t, 1<<20, 64<<20)
		client := fixture.connectObserved(t, alice())
		instanceID := fixture.enable(t, client, "user:alice", "echo")

		callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})

		fixture.change(func(template *v1alpha1.MCPServerTemplate) { template.Generation++ })
		_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET generation = $1 WHERE id = $2`, fixture.template.Generation, instanceID)
		require.NoError(t, err)

		for range 2 {
			callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
		}

		assert.Equal(t, 2, fixture.upstream.callCount("echo"))
		assert.Equal(t, []map[string]any{{"cache": "miss"}, {"cache": "miss"}, {"cache": "hit"}}, fixture.cacheResults(t, "echo"))
	})

	t.Run("a call forwarding during the change stores under the old generation", func(t *testing.T) {
		fixture := newCacheFixture(t, 1<<20, 64<<20)
		fixture.change(func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools["slow"] = v1alpha1.ToolOverride{Cache: &v1alpha1.ToolCache{}}
		})

		client := fixture.connectObserved(t, alice())
		instanceID := fixture.enableWithTools(t, client, "parts", "user:alice", "slow").ID
		readOnlySlow := `[{"name":"slow","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]`
		_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET tools = $1 WHERE id = $2`, readOnlySlow, instanceID)
		require.NoError(t, err)

		oldGeneration := fixture.template.Generation
		done := make(chan *mcp.CallToolResult, 1)

		go func() {
			result, _ := client.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "parts__slow", Arguments: map[string]any{}})
			done <- result
		}()

		<-fixture.upstream.started

		changed := fixture.template.DeepCopy()
		changed.Generation++
		fixture.catalog.Set(changed, nil)
		_, err = fixture.pool.Exec(t.Context(), `UPDATE instances SET generation = $1 WHERE id = $2`, changed.Generation, instanceID)
		require.NoError(t, err)

		close(fixture.upstream.release)
		require.False(t, (<-done).IsError)

		var stored int64
		require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT generation FROM tool_result_cache`).Scan(&stored))
		assert.Equal(t, oldGeneration, stored)

		result := callTool(t, client.session, "parts__slow", map[string]any{})
		_, found := cacheAge(result)
		assert.False(t, found, "the new generation doesn't read the in-flight call's entry")
		assert.Equal(t, 2, fixture.upstream.callCount("slow"))
	})

	t.Run("a template deleted and created again doesn't read the old entries", func(t *testing.T) {
		fixture := newCacheFixture(t, 1<<20, 64<<20)
		client := fixture.connectObserved(t, alice())
		instanceID := fixture.enable(t, client, "user:alice", "echo")

		callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})

		fixture.catalog.Delete(types.NamespacedName{Namespace: templateNamespace, Name: "parts"})
		fixture.change(func(template *v1alpha1.MCPServerTemplate) { template.UID = "uid-parts-again" })
		_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET template_uid = $1 WHERE id = $2`, "uid-parts-again", instanceID)
		require.NoError(t, err)

		for range 2 {
			callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
		}

		assert.Equal(t, 2, fixture.upstream.callCount("echo"))
		assert.Equal(t, []map[string]any{{"cache": "miss"}, {"cache": "miss"}, {"cache": "hit"}}, fixture.cacheResults(t, "echo"))
	})
}

func TestInstanceReplaceKeepsEntries(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	oldID := fixture.enable(t, client, "user:alice", "echo")

	callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})

	fixture.stopInstance(t, oldID)
	newID := fixture.readyInstance(t, fixture.template, "alice", fixture.template.Generation)
	fixture.setReadOnly(t, newID, "echo")

	result := callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})
	_, found := cacheAge(result)
	assert.True(t, found, "the replacement at the same generation serves the entry")
	assert.Equal(t, 1, fixture.upstream.callCount("echo"))

	results := fixture.auditRows(t, "tool.result")
	assert.Equal(t, newID, results[len(results)-1].Details["instance"])
}

func TestOnCallServerStartsThenHits(t *testing.T) {
	fixture := newCacheFixture(t, 1<<20, 64<<20)
	client := fixture.connectObserved(t, alice())
	oldID := fixture.enable(t, client, "user:alice", "echo")

	callTool(t, client.session, "parts__echo", map[string]any{"text": "x"})

	// The server now starts on a call, and its instance stopped.
	fixture.learn(t, string(fixture.template.UID), fixture.template.Generation, "echo")
	fixture.change(func(template *v1alpha1.MCPServerTemplate) { template.Spec.Start = catalog.StartOnCall })
	fixture.stopInstance(t, oldID)

	done := make(chan *mcp.CallToolResult, 1)

	go func() {
		result, _ := client.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "parts__echo", Arguments: map[string]any{"text": "x"}})
		done <- result
	}()

	var started db.Instance

	require.Eventually(t, func() bool {
		for _, instance := range fixture.instancesOf(t, fixture.template) {
			if instance.ID != oldID {
				started = instance

				return true
			}
		}

		return false
	}, waitTimeout, 10*time.Millisecond)

	fixture.markReadyReadOnly(t, started.ID, "echo")

	result := <-done
	require.NotNil(t, result)
	require.False(t, result.IsError, resultText(result))

	_, found := cacheAge(result)
	assert.True(t, found, "the call hits once its instance started")
	assert.Equal(t, 1, fixture.upstream.callCount("echo"))
}
