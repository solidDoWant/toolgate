package toolcatalog_test

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

func template(mutate func(*v1alpha1.MCPServerTemplateSpec)) *v1alpha1.MCPServerTemplate {
	result := &v1alpha1.MCPServerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "github"}}
	mutate(&result.Spec)

	return result
}

func upstreamTools() []*mcp.Tool {
	return []*mcp.Tool{
		{Name: "create_issue", Description: "Create.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"repo": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}},
		}},
		{Name: "list_issues", Description: "List."},
		{Name: "delete_repository", Description: "Delete."},
	}
}

func names(tools []toolcatalog.Tool) [][2]string {
	var result [][2]string
	for _, tool := range tools {
		result = append(result, [2]string{tool.UpstreamName, tool.ExposedName})
	}

	return result
}

func TestCurateFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter *v1alpha1.ToolFilter
		want   [][2]string
	}{
		{name: "no filter", want: [][2]string{{"create_issue", "create_issue"}, {"list_issues", "list_issues"}, {"delete_repository", "delete_repository"}}},
		{name: "allow", filter: &v1alpha1.ToolFilter{Allow: []string{"list_issues"}}, want: [][2]string{{"list_issues", "list_issues"}}},
		{name: "empty allow hides all", filter: &v1alpha1.ToolFilter{Allow: []string{}}},
		{name: "deny", filter: &v1alpha1.ToolFilter{Deny: []string{"delete_repository"}}, want: [][2]string{{"create_issue", "create_issue"}, {"list_issues", "list_issues"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := toolcatalog.Curate(template(func(spec *v1alpha1.MCPServerTemplateSpec) { spec.ToolFilter = test.filter }), upstreamTools())
			assert.Equal(t, test.want, names(got))
		})
	}
}

func TestCurateOverridesApplyToCopies(t *testing.T) {
	spec := template(func(spec *v1alpha1.MCPServerTemplateSpec) {
		spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {
			Name: "open_issue", Description: "Open one.",
			InputDescriptions: map[string]string{"repo": "owner/name", "missing": "ignored"},
		}}
	})
	upstream := upstreamTools()

	got := toolcatalog.Curate(spec, upstream)
	require.Len(t, got, 3)
	assert.Equal(t, "create_issue", got[0].UpstreamName)
	assert.Equal(t, "open_issue", got[0].ExposedName)
	assert.Equal(t, "open_issue", got[0].Tool.Name)
	assert.Equal(t, "Open one.", got[0].Tool.Description)

	properties := got[0].Tool.InputSchema.(map[string]any)["properties"].(map[string]any)
	assert.Equal(t, "owner/name", properties["repo"].(map[string]any)["description"])
	assert.NotContains(t, properties, "missing")
	assert.NotContains(t, properties["title"], "description")

	// The input is unchanged.
	assert.Equal(t, "create_issue", upstream[0].Name)
	assert.Equal(t, "Create.", upstream[0].Description)

	original := upstream[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, original["repo"], "description")

	// Changing a result doesn't change the input either.
	got[1].Tool.Description = "changed"
	assert.Equal(t, "List.", upstream[1].Description)
}

func TestReverseMapAndUpstream(t *testing.T) {
	spec := template(func(spec *v1alpha1.MCPServerTemplateSpec) {
		spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"delete_repository", "secret"}}
		spec.Tools = map[string]v1alpha1.ToolOverride{
			"create_issue": {Name: "open_issue"},
			"secret":       {Name: "other"},
		}
	})

	assert.Equal(t, map[string]string{"open_issue": "create_issue", "other": "secret"}, toolcatalog.ReverseMap(spec))

	tests := []struct {
		exposed  string
		upstream string
		offered  bool
	}{
		{exposed: "open_issue", upstream: "create_issue", offered: true},
		{exposed: "create_issue", upstream: "create_issue"},
		{exposed: "list_issues", upstream: "list_issues", offered: true},
		{exposed: "delete_repository", upstream: "delete_repository"},
		{exposed: "other", upstream: "secret"},
	}

	for _, test := range tests {
		t.Run(test.exposed, func(t *testing.T) {
			upstream, offered := toolcatalog.Upstream(spec, test.exposed)
			assert.Equal(t, test.upstream, upstream)
			assert.Equal(t, test.offered, offered)
		})
	}
}

func TestCheck(t *testing.T) {
	tools := func(names ...string) []*mcp.Tool {
		var list []*mcp.Tool
		for _, name := range names {
			list = append(list, &mcp.Tool{Name: name})
		}

		return list
	}

	tests := []struct {
		name  string
		spec  func(*v1alpha1.MCPServerTemplateSpec)
		tools []*mcp.Tool
		want  string
	}{
		{name: "no overrides", spec: func(*v1alpha1.MCPServerTemplateSpec) {}, tools: tools("a", "b")},
		{
			name: "override collides with another tool",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}
			},
			tools: tools("create_issue", "open_issue"),
			want:  "Tool name `open_issue` of `open_issue` collides with another tool.",
		},
		{
			name: "two overrides collide",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: "same"}, "b": {Name: "same"}}
			},
			tools: tools("a", "b"),
			want:  "Tool name `same` of `b` collides with another tool.",
		},
		{
			name: "collision with a hidden tool is no collision when the override name is its own",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"create_issue"}}
				spec.Tools = map[string]v1alpha1.ToolOverride{"other": {Name: "x"}}
			},
			tools: tools("create_issue", "other"),
		},
		{
			name: "override name is a hidden tool under allow",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"create_issue"}}
				spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "list_issues"}}
			},
			tools: tools("create_issue", "list_issues"),
			want:  "Tool name `list_issues` of `create_issue` is the name of a hidden tool.",
		},
		{
			name: "override name is the name of a tool hidden by deny",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"list_issues"}}
				spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "list_issues"}}
			},
			tools: tools("create_issue", "list_issues"),
			want:  "Tool name `list_issues` of `create_issue` is the name of a hidden tool.",
		},
		{
			name: "prefix limit uses the exposed name",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{"short": {Name: strings.Repeat("x", 57)}}
			},
			tools: tools("short"),
			want:  "Tool `" + strings.Repeat("x", 57) + "` is longer than 64 characters with its `github__` prefix.",
		},
		{
			name: "a long upstream name is fine when renamed to 56",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{strings.Repeat("y", 90): {Name: strings.Repeat("x", 56)}}
			},
			tools: tools(strings.Repeat("y", 90)),
		},
		{
			name: "a long hidden tool is fine",
			spec: func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{strings.Repeat("y", 90)}}
			},
			tools: tools(strings.Repeat("y", 90)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, toolcatalog.Check(template(test.spec), test.tools))
		})
	}
}
