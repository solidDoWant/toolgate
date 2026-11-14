package catalog_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

func cachedTemplate(scope string, cache v1alpha1.ToolCache) *v1alpha1.MCPServerTemplate {
	template := newTemplate("parts", scope)
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"search_parts": {Cache: &cache}}

	return template
}

func TestCacheScopeBroaderIsInvalid(t *testing.T) {
	template := cachedTemplate(catalog.ScopePerUser, v1alpha1.ToolCache{Scope: catalog.ScopeShared})

	assert.Equal(t,
		"`tools.search_parts.cache.scope` is `shared`, which is broader than the template's scope `per-user`. A cache scope can only narrow the template's scope.",
		catalog.Reason(catalog.Validator{}.Validate(template)))

	for _, scope := range []string{catalog.ScopePerUser, catalog.ScopePerSession} {
		t.Run(scope+" is allowed on a per-user template", func(t *testing.T) {
			assert.Empty(t, catalog.Validator{}.Validate(cachedTemplate(catalog.ScopePerUser, v1alpha1.ToolCache{Scope: scope})))
		})
	}

	t.Run("per-user is broader than a per-session template", func(t *testing.T) {
		problems := catalog.Validator{}.Validate(cachedTemplate(catalog.ScopePerSession, v1alpha1.ToolCache{Scope: catalog.ScopePerUser}))
		assert.Contains(t, catalog.Reason(problems), "broader than the template's scope `per-session`")
	})
}

func TestCacheNormalizeOfIgnoredArgumentIsInvalid(t *testing.T) {
	template := cachedTemplate(catalog.ScopePerUser, v1alpha1.ToolCache{
		IgnoreArgs: []string{"reason"},
		Normalize:  map[string]v1alpha1.NormalizeOperations{"reason": {"trim"}, "query": {"trim"}},
	})

	assert.Equal(t,
		"`tools.search_parts.cache.normalize.reason` names an argument that `ignoreArgs` leaves out.",
		catalog.Reason(catalog.Validator{}.Validate(template)))
}

func TestCacheSettingsBounds(t *testing.T) {
	duration := func(value time.Duration) *metav1.Duration { return &metav1.Duration{Duration: value} }
	names := func(count int) []string {
		result := make([]string, count)
		for index := range result {
			result[index] = fmt.Sprintf("argument-%d", index)
		}

		return result
	}
	normalize := func(count int) map[string]v1alpha1.NormalizeOperations {
		result := map[string]v1alpha1.NormalizeOperations{}
		for _, name := range names(count) {
			result[name] = v1alpha1.NormalizeOperations{"trim"}
		}

		return result
	}

	tests := []struct {
		name  string
		cache v1alpha1.ToolCache
		want  string
	}{
		{name: "empty cache", cache: v1alpha1.ToolCache{}},
		{name: "ttl of 1s", cache: v1alpha1.ToolCache{TTL: duration(time.Second)}},
		{name: "ttl under 1s", cache: v1alpha1.ToolCache{TTL: duration(time.Second - time.Millisecond)}, want: "`tools.search_parts.cache.ttl` is 999ms, outside 1s to 24h."},
		{name: "ttl of 24h", cache: v1alpha1.ToolCache{TTL: duration(24 * time.Hour)}},
		{name: "ttl over 24h", cache: v1alpha1.ToolCache{TTL: duration(24*time.Hour + time.Second)}, want: "`tools.search_parts.cache.ttl` is 24h0m1s, outside 1s to 24h."},
		{name: "unknown scope", cache: v1alpha1.ToolCache{Scope: "global"}, want: "`tools.search_parts.cache.scope` is `global`, which is not one of per-session, per-user, shared."},
		{name: "64 ignoreArgs", cache: v1alpha1.ToolCache{IgnoreArgs: names(64)}},
		{name: "65 ignoreArgs", cache: v1alpha1.ToolCache{IgnoreArgs: names(65)}, want: "`tools.search_parts.cache.ignoreArgs` has 65 names, more than the limit of 64."},
		{name: "ignoreArgs name of 128", cache: v1alpha1.ToolCache{IgnoreArgs: []string{strings.Repeat("a", 128)}}},
		{name: "ignoreArgs name of 129", cache: v1alpha1.ToolCache{IgnoreArgs: []string{strings.Repeat("a", 129)}}, want: "`tools.search_parts.cache.ignoreArgs` has a name that is not 1 to 128 characters long."},
		{name: "empty ignoreArgs name", cache: v1alpha1.ToolCache{IgnoreArgs: []string{""}}, want: "`tools.search_parts.cache.ignoreArgs` has a name that is not 1 to 128 characters long."},
		{name: "repeated ignoreArgs name", cache: v1alpha1.ToolCache{IgnoreArgs: []string{"a", "b", "a"}}, want: "`tools.search_parts.cache.ignoreArgs` names `a` more than once."},
		{name: "64 normalize entries", cache: v1alpha1.ToolCache{Normalize: normalize(64)}},
		{name: "65 normalize entries", cache: v1alpha1.ToolCache{Normalize: normalize(65)}, want: "`tools.search_parts.cache.normalize` has 65 entries, more than the limit of 64."},
		{name: "normalize name of 128", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{strings.Repeat("a", 128): {"trim"}}}},
		{name: "normalize name of 129", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{strings.Repeat("a", 129): {"trim"}}}, want: "`tools.search_parts.cache.normalize` has a name that is not 1 to 128 characters long."},
		{name: "empty normalize name", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"": {"trim"}}}, want: "`tools.search_parts.cache.normalize` has a name that is not 1 to 128 characters long."},
		{name: "three operations", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"query": {"collapseSpace", "lowercase", "trim"}}}},
		{name: "four operations", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"query": {"trim", "lowercase", "collapseSpace", "trim"}}}, want: "`tools.search_parts.cache.normalize.query` must list 1 to 3 operations."},
		{name: "no operations", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"query": {}}}, want: "`tools.search_parts.cache.normalize.query` must list 1 to 3 operations."},
		{name: "unknown operation", cache: v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"query": {"uppercase"}}}, want: "`tools.search_parts.cache.normalize.query` has the operation `uppercase`, which is not one of trim, lowercase, collapseSpace."},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := catalog.Validator{}.Validate(cachedTemplate(catalog.ScopePerUser, test.cache))
			if test.want == "" {
				assert.Empty(t, problems)

				return
			}

			assert.Equal(t, test.want, catalog.Reason(problems))
		})
	}
}
