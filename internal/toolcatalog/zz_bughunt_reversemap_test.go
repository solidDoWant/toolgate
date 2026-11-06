package toolcatalog_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// A hidden tool whose override name equals a visible tool's upstream name:
// the visible tool is listed under its own name, but a call of that name maps
// to the hidden tool and is refused.
func TestBughuntHiddenOverrideShadowsVisibleTool(t *testing.T) {
	template := &v1alpha1.MCPServerTemplate{}
	template.Name = "srv"
	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"old_search"}}
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"old_search": {Name: "search"}}

	upstream := []*mcp.Tool{{Name: "old_search"}, {Name: "search"}}

	require.Empty(t, toolcatalog.Check(template, upstream), "the tool list checks pass")

	curated := toolcatalog.Curate(template, upstream)
	require.Len(t, curated, 1)
	assert.Equal(t, "search", curated[0].ExposedName, "search is listed")

	mapped, offered := toolcatalog.Upstream(template, "search")
	assert.True(t, offered, "a listed tool must be callable by its listed name")
	assert.Equal(t, "search", mapped)
}

// Two overrides with the same name, only one tool present (the other absent
// or hidden): the reverse map picks either at random.
func TestBughuntDuplicateOverrideNameIsNondeterministic(t *testing.T) {
	template := &v1alpha1.MCPServerTemplate{}
	template.Name = "srv"
	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"b"}}
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"a": {Name: "x"}, "b": {Name: "x"}}

	upstream := []*mcp.Tool{{Name: "a"}, {Name: "b"}}
	require.Empty(t, toolcatalog.Check(template, upstream), "the tool list checks pass")

	curated := toolcatalog.Curate(template, upstream)
	require.Len(t, curated, 1)
	require.Equal(t, "x", curated[0].ExposedName)

	refused := 0

	for range 200 {
		mapped, offered := toolcatalog.Upstream(template, "x")
		if !offered || mapped != "a" {
			refused++
		}
	}

	assert.Zero(t, refused, "x is listed (as a), so every call of x must map to a and be offered")
}
