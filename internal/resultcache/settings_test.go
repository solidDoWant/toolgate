package resultcache_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/resultcache"
)

// upstreamTool decodes one entry of an instance's tool list, as the call
// path reads it from instances.tools.
func upstreamTool(t *testing.T, entry string) *mcp.Tool {
	t.Helper()

	tool := &mcp.Tool{}
	require.NoError(t, json.Unmarshal([]byte(entry), tool))

	return tool
}

func TestLifetime(t *testing.T) {
	hour := &metav1.Duration{Duration: time.Hour}

	tests := []struct {
		name  string
		cache v1alpha1.ToolCache
		tool  string
		want  time.Duration
	}{
		{name: "the template's ttl wins over the server's", cache: v1alpha1.ToolCache{TTL: hour},
			tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"10m"}}}`, want: time.Hour},
		{name: "the server's suggestion applies without a template ttl",
			tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"10m"}}}`, want: 10 * time.Minute},
		{name: "a suggestion of 1s applies", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"1s"}}}`, want: time.Second},
		{name: "a suggestion of 24h applies", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"24h"}}}`, want: 24 * time.Hour},
		{name: "a suggestion under 1s is ignored", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"999ms"}}}`, want: 5 * time.Minute},
		{name: "a suggestion over 24h is ignored", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"24h0m1s"}}}`, want: 5 * time.Minute},
		{name: "a suggestion that doesn't parse is ignored", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":"an hour"}}}`, want: 5 * time.Minute},
		{name: "a numeric suggestion is ignored", tool: `{"name":"a","_meta":{"toolgate/cache":{"ttl":3600}}}`, want: 5 * time.Minute},
		{name: "a suggestion that is not an object is ignored", tool: `{"name":"a","_meta":{"toolgate/cache":"1h"}}`, want: 5 * time.Minute},
		{name: "no suggestion gives 5 minutes", tool: `{"name":"a"}`, want: 5 * time.Minute},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, resultcache.Lifetime(test.cache, upstreamTool(t, test.tool)))
		})
	}

	t.Run("no tool entry gives 5 minutes", func(t *testing.T) {
		assert.Equal(t, 5*time.Minute, resultcache.Lifetime(v1alpha1.ToolCache{}, nil))
	})
}

func TestCacheScope(t *testing.T) {
	assert.Equal(t, "per-session", resultcache.CacheScope(v1alpha1.ToolCache{Scope: "per-session"}, "per-user"))
	assert.Equal(t, "per-user", resultcache.CacheScope(v1alpha1.ToolCache{}, "per-user"))
	assert.Equal(t, "shared", resultcache.CacheScope(v1alpha1.ToolCache{}, "shared"))
}

func TestEligibility(t *testing.T) {
	readOnly := `{"name":"search_parts","annotations":{"readOnlyHint":true}}`
	cache := &v1alpha1.ToolCache{}

	tests := []struct {
		name               string
		server             string
		cache              *v1alpha1.ToolCache
		templateGeneration int64
		instanceGeneration int64
		tool               string
		want               resultcache.Decision
	}{
		{name: "a read-only tool of a current instance is eligible", server: "parts", cache: cache,
			templateGeneration: 3, instanceGeneration: 3, tool: readOnly, want: resultcache.Decision{Use: true}},
		{name: "the built-in server is never eligible", server: "toolgate", cache: cache,
			templateGeneration: 3, instanceGeneration: 3, tool: readOnly, want: resultcache.Decision{}},
		{name: "a tool without cache settings has no cache step", server: "parts",
			templateGeneration: 3, instanceGeneration: 3, tool: readOnly, want: resultcache.Decision{}},
		{name: "an outdated instance is skipped", server: "parts", cache: cache,
			templateGeneration: 4, instanceGeneration: 3, tool: readOnly, want: resultcache.Decision{Skip: resultcache.SkipOutdatedInstance}},
		{name: "a false readOnlyHint is skipped", server: "parts", cache: cache, templateGeneration: 3, instanceGeneration: 3,
			tool: `{"name":"search_parts","annotations":{"readOnlyHint":false}}`, want: resultcache.Decision{Skip: resultcache.SkipNotReadOnly}},
		{name: "a missing readOnlyHint is skipped", server: "parts", cache: cache, templateGeneration: 3, instanceGeneration: 3,
			tool: `{"name":"search_parts","annotations":{"title":"Search"}}`, want: resultcache.Decision{Skip: resultcache.SkipNotReadOnly}},
		{name: "missing annotations are skipped", server: "parts", cache: cache, templateGeneration: 3, instanceGeneration: 3,
			tool: `{"name":"search_parts"}`, want: resultcache.Decision{Skip: resultcache.SkipNotReadOnly}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resultcache.Eligibility(test.server, test.cache, test.templateGeneration, test.instanceGeneration, upstreamTool(t, test.tool))
			assert.Equal(t, test.want, got)
		})
	}

	t.Run("a tool missing from the instance's list is skipped", func(t *testing.T) {
		assert.Equal(t, resultcache.Decision{Skip: resultcache.SkipNotReadOnly},
			resultcache.Eligibility("parts", cache, 3, 3, nil))
	})

	t.Run("a tool that gets the hint back is eligible again", func(t *testing.T) {
		lost := upstreamTool(t, `{"name":"search_parts","annotations":{"readOnlyHint":false}}`)
		assert.Equal(t, resultcache.Decision{Skip: resultcache.SkipNotReadOnly}, resultcache.Eligibility("parts", cache, 3, 3, lost))

		back := upstreamTool(t, readOnly)
		assert.Equal(t, resultcache.Decision{Use: true}, resultcache.Eligibility("parts", cache, 3, 3, back))
	})

	t.Run("an instance of the last valid generation of an invalid template is eligible", func(t *testing.T) {
		template := &v1alpha1.MCPServerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "parts", Namespace: "catalog", Generation: 3},
			Spec: v1alpha1.MCPServerTemplateSpec{
				Scope:     catalog.ScopePerUser,
				Namespace: catalog.NamespacePerInstance,
				Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080},
				Tools:     map[string]v1alpha1.ToolOverride{"search_parts": {Cache: &v1alpha1.ToolCache{}}},
			},
		}

		store := catalog.New()
		store.Set(template, catalog.Validator{}.Validate(template))

		invalid := template.DeepCopy()
		invalid.Generation = 4
		invalid.Spec.Tools["search_parts"] = v1alpha1.ToolOverride{Cache: &v1alpha1.ToolCache{Scope: catalog.ScopeShared}}

		problems := catalog.Validator{}.Validate(invalid)
		require.NotEmpty(t, problems)
		store.Set(invalid, problems)

		running, found := store.Running(client.ObjectKeyFromObject(invalid))
		require.True(t, found)
		require.Equal(t, int64(3), running.Generation)

		got := resultcache.Eligibility("parts", running.Spec.Tools["search_parts"].Cache, running.Generation, 3, upstreamTool(t, readOnly))
		assert.Equal(t, resultcache.Decision{Use: true}, got)
	})
}
