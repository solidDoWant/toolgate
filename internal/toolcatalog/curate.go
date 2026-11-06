// Package toolcatalog curates the tools of a server: it hides tools, renames
// them and rewrites their descriptions as the server's template says, and it
// checks the result (docs/design/phase-4.md §6).
package toolcatalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Tool list limits (docs/design/phase-1.md §3).
const (
	// MaxTools is the most tools one server may list.
	MaxTools = 512
	// MaxToolNameLength is the longest `<server>__<tool>` name.
	MaxToolNameLength = 64
	// MaxDeclaredToolsBytes is the largest encoded size of declared tools.
	MaxDeclaredToolsBytes = 1 << 20
)

// ExposedNamePattern is the pattern an override name must match.
var ExposedNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// maxNameInReason bounds a tool name copied into a reason.
const maxNameInReason = 512

// Tool is a tool after curation.
type Tool struct {
	// Tool is a deep copy of the upstream tool with the overrides applied.
	// Its Name is the exposed name.
	Tool *mcp.Tool
	// UpstreamName is the name the upstream server gave the tool.
	UpstreamName string
	// ExposedName is the name the tool is offered under.
	ExposedName string
}

// Hidden reports whether the template's tool filter hides the tool with the
// upstream name name.
func Hidden(template *v1alpha1.MCPServerTemplate, name string) bool {
	filter := template.Spec.ToolFilter
	if filter == nil {
		return false
	}

	if filter.Allow != nil {
		return !slices.Contains(filter.Allow, name)
	}

	return slices.Contains(filter.Deny, name)
}

// ExposedName returns the name the tool with the upstream name name is
// offered under.
func ExposedName(template *v1alpha1.MCPServerTemplate, name string) string {
	if override := template.Spec.Tools[name].Name; override != "" {
		return override
	}

	return name
}

// Curate returns the tools of upstream that the template exposes, in order,
// with the overrides applied to deep copies. The input is never changed.
func Curate(template *v1alpha1.MCPServerTemplate, upstream []*mcp.Tool) []Tool {
	curated := make([]Tool, 0, len(upstream))

	for _, tool := range upstream {
		if tool == nil || Hidden(template, tool.Name) {
			continue
		}

		override := template.Spec.Tools[tool.Name]
		copied := copyTool(tool)

		if override.Name != "" {
			copied.Name = override.Name
		}

		if override.Description != "" {
			copied.Description = override.Description
		}

		describeInputs(copied, override.InputDescriptions)

		curated = append(curated, Tool{Tool: copied, UpstreamName: tool.Name, ExposedName: copied.Name})
	}

	return curated
}

// ReverseMap maps each override name of the template to the upstream name of
// its tool. It is built from the overrides alone, so it works while no
// instance runs.
func ReverseMap(template *v1alpha1.MCPServerTemplate) map[string]string {
	reverse := map[string]string{}

	for upstream, override := range template.Spec.Tools {
		if override.Name != "" {
			reverse[override.Name] = upstream
		}
	}

	return reverse
}

// Upstream maps an exposed name to the upstream name of its tool, and
// reports whether the tool is offered under that name. An override name maps
// to its tool. Any other name maps to itself, and is not offered when that
// tool is hidden or has an override name. The map is built from the template
// alone.
func Upstream(template *v1alpha1.MCPServerTemplate, exposed string) (string, bool) {
	if upstream, found := ReverseMap(template)[exposed]; found {
		if Hidden(template, upstream) {
			return upstream, false
		}

		return upstream, true
	}

	if Hidden(template, exposed) || template.Spec.Tools[exposed].Name != "" {
		return exposed, false
	}

	return exposed, true
}

// Check applies the tool list checks to the curated form of upstream and
// returns the reason a check fails, or "". The server name is the
// template's.
func Check(template *v1alpha1.MCPServerTemplate, upstream []*mcp.Tool) string {
	if len(upstream) > MaxTools {
		return fmt.Sprintf("The server lists %d tools, more than the limit of %d.", len(upstream), MaxTools)
	}

	server := template.Name
	curated := Curate(template, upstream)

	for _, tool := range curated {
		if len(server)+len("__")+len(tool.ExposedName) > MaxToolNameLength {
			return fmt.Sprintf("Tool `%s` is longer than %d characters with its `%s__` prefix.",
				shorten(tool.ExposedName), MaxToolNameLength, server)
		}
	}

	hidden := map[string]bool{}

	for _, tool := range upstream {
		if tool != nil && Hidden(template, tool.Name) {
			hidden[tool.Name] = true
		}
	}

	seen := map[string]bool{}

	for _, tool := range curated {
		if tool.ExposedName != tool.UpstreamName && hidden[tool.ExposedName] {
			return fmt.Sprintf("Tool name `%s` of `%s` is the name of a hidden tool.",
				shorten(tool.ExposedName), shorten(tool.UpstreamName))
		}

		if seen[tool.ExposedName] {
			return fmt.Sprintf("Tool name `%s` of `%s` collides with another tool.",
				shorten(tool.ExposedName), shorten(tool.UpstreamName))
		}

		seen[tool.ExposedName] = true
	}

	return ""
}

func shorten(name string) string {
	if len(name) <= maxNameInReason {
		return name
	}

	return name[:maxNameInReason]
}

// copyTool deep-copies a tool through its JSON form, which is also how it is
// stored. A tool that doesn't encode is copied shallowly; it has no schema
// that curation would change.
func copyTool(tool *mcp.Tool) *mcp.Tool {
	encoded, err := json.Marshal(tool)
	if err != nil {
		shallow := *tool

		return &shallow
	}

	var copied mcp.Tool
	if err := json.Unmarshal(encoded, &copied); err != nil {
		shallow := *tool

		return &shallow
	}

	return &copied
}

// describeInputs sets the description of the top-level input properties that
// the schema has. Other names are ignored.
func describeInputs(tool *mcp.Tool, descriptions map[string]string) {
	if len(descriptions) == 0 {
		return
	}

	schema, isMap := tool.InputSchema.(map[string]any)
	if !isMap {
		return
	}

	properties, isMap := schema["properties"].(map[string]any)
	if !isMap {
		return
	}

	for name, description := range descriptions {
		if property, isMap := properties[name].(map[string]any); isMap {
			property["description"] = description
		}
	}
}
