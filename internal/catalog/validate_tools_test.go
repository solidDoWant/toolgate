package catalog_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

func TestValidateToolFields(t *testing.T) {
	declared := func(names ...string) []v1alpha1.DeclaredTool {
		var tools []v1alpha1.DeclaredTool
		for _, name := range names {
			tools = append(tools, v1alpha1.DeclaredTool{Name: name})
		}

		return tools
	}
	many := func(count int) []v1alpha1.DeclaredTool {
		var tools []v1alpha1.DeclaredTool
		for index := range count {
			tools = append(tools, v1alpha1.DeclaredTool{Name: "t" + strings.Repeat("x", index%5) + string(rune('a'+index/26%26)) + string(rune('a'+index%26)) + string(rune('a'+index/676))})
		}

		return tools
	}
	names := func(count, length int) []string {
		var list []string
		for index := range count {
			list = append(list, strings.Repeat("n", length-3)+string(rune('a'+index/100))+string(rune('a'+index/10%10))+string(rune('a'+index%10)))
		}

		return list
	}
	schema := func(size int) *runtime.RawExtension {
		return &runtime.RawExtension{Raw: []byte(`{"type":"object","description":"` + strings.Repeat("d", size) + `"}`)}
	}

	tests := []struct {
		name   string
		change func(*v1alpha1.MCPServerTemplate)
		want   string
	}{
		{name: "defaults", change: func(*v1alpha1.MCPServerTemplate) {}},
		{name: "start onEnable", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.Start = "onEnable" }},
		{name: "start onCall", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.Start = "onCall" }},
		{name: "start unknown", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.Start = "always" }, want: "start"},
		{name: "512 declared tools", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.DeclaredTools = many(512) }},
		{name: "513 declared tools", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.DeclaredTools = many(513) }, want: "513 tools, more than the limit of 512"},
		{name: "declared names repeat", change: func(template *v1alpha1.MCPServerTemplate) { template.Spec.DeclaredTools = declared("a", "b", "a") }, want: "names `a` twice"},
		{name: "declared tools under 1 MiB", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "a", InputSchema: schema(1<<20 - 200)}}
		}},
		{name: "declared tools over 1 MiB", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "a", InputSchema: schema(1 << 20)}}
		}, want: "more than the limit of 1 MiB"},
		{name: "declared input schema not an object", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "a", InputSchema: &runtime.RawExtension{Raw: []byte(`[1]`)}}}
		}, want: "not an object"},
		{name: "declared name at 64 with prefix", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = declared(strings.Repeat("x", 56))
		}},
		{name: "declared name at 65 with prefix", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = declared(strings.Repeat("x", 57))
		}, want: "longer than 64 characters with its `github__` prefix"},
		{name: "declared tools collide after curation", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = declared("create_issue", "open_issue")
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}
		}, want: "Tool name `open_issue` of `open_issue` collides with another tool."},
		{name: "declared tools with an override name of a hidden tool", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.DeclaredTools = declared("create_issue", "list_issues")
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"create_issue"}}
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "list_issues"}}
		}, want: "Tool name `list_issues` of `create_issue` is the name of a hidden tool."},

		{name: "filter with allow", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"a"}}
		}},
		{name: "filter with neither", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{}
		}, want: "exactly one of `allow` and `deny`"},
		{name: "filter with both", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"a"}, Deny: []string{"b"}}
		}, want: "exactly one of `allow` and `deny`"},
		{name: "512 filter names", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: names(512, 10)}
		}},
		{name: "513 filter names", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: names(513, 10)}
		}, want: "`toolFilter.deny` has 513 names"},
		{name: "filter name of 128", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: names(1, 128)}
		}},
		{name: "filter name of 129", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: names(1, 129)}
		}, want: "`toolFilter.allow` has a name longer than 128"},

		{name: "override name with a space", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: "open issue"}}
		}, want: "`tools.a.name` must match ^[A-Za-z0-9_.-]{1,128}$"},
		{name: "override name valid characters", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: "Open_issue.v-2"}}
		}},
		{name: "override name 56 characters", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: strings.Repeat("x", 56)}}
		}},
		{name: "override name 57 characters", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: strings.Repeat("x", 57)}}
		}, want: "longer than 64 characters with its `github__` prefix"},
		{name: "override name 129 characters", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: strings.Repeat("x", 129)}}
		}, want: "`tools.a.name` must match"},
		{name: "override name equals a denied name", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"list_issues"}}
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "list_issues"}}
		}, want: "Tool name `list_issues` of `create_issue` is the name of a hidden tool."},
		{name: "two overrides share a name, one tool hidden", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"b"}}
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: "x"}, "b": {Name: "x"}}
		}, want: "Tool name `x` is the name of both `a` and `b`."},
		{name: "override name equals an allowed name", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"list_issues"}}
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "list_issues"}}
		}},
		{name: "description of 4096", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Description: strings.Repeat("d", 4096)}}
		}},
		{name: "description of 4097", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Description: strings.Repeat("d", 4097)}}
		}, want: "`tools.a.description` is longer than 4096"},
		{name: "input description of 4097", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {InputDescriptions: map[string]string{"p": strings.Repeat("d", 4097)}}}
		}, want: "inputDescriptions"},
		{name: "64 input descriptions", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {InputDescriptions: inputDescriptions(64)}}
		}},
		{name: "65 input descriptions", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {InputDescriptions: inputDescriptions(65)}}
		}, want: "inputDescriptions"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := newTemplate("github", catalog.ScopePerUser)
			test.change(template)

			problems := catalog.Validator{}.Validate(template)
			if test.want == "" {
				assert.Empty(t, problems)

				return
			}

			assert.Contains(t, catalog.Reason(problems), test.want)
		})
	}
}

func inputDescriptions(count int) map[string]string {
	descriptions := map[string]string{}
	for index := range count {
		descriptions[strings.Repeat("p", index+1)] = "d"
	}

	return descriptions
}

func TestReservedServerNameIsInvalidAndCountsAsDeleted(t *testing.T) {
	template := newTemplate("toolgate", catalog.ScopeShared)

	problems := catalog.Validator{}.Validate(template)
	assert.Contains(t, catalog.Reason(problems), "The server name `toolgate` is reserved for the built-in server.")

	// Even a catalog told the template is valid serves no generation for it.
	store := catalog.New()
	store.Set(template, nil)

	_, found := store.Running(keyOf(template))
	assert.False(t, found)

	store.Set(template, problems)

	_, err := store.Offered("toolgate")

	var invalid *catalog.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, "The server name `toolgate` is reserved for the built-in server.", invalid.Reason)
}

func TestInvalidCurationKeepsLastValidGeneration(t *testing.T) {
	template := newTemplate("github", catalog.ScopePerUser)
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}

	store := catalog.New()
	store.Set(template, catalog.Validator{}.Validate(template))

	invalid := template.DeepCopy()
	invalid.Generation = 2
	invalid.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "bad name"}}
	problems := catalog.Validator{}.Validate(invalid)
	require.NotEmpty(t, problems)
	store.Set(invalid, problems)

	running, found := store.Running(keyOf(template))
	require.True(t, found)
	assert.Equal(t, "open_issue", running.Spec.Tools["create_issue"].Name)
}
