package toolcatalog

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Learned is the stored upstream tool list of one template generation.
type Learned struct {
	Generation int64
	// Tools is the JSON list as the instance served it, before curation.
	Tools []byte
}

// DeclaredTools returns the template's declared tools as upstream tools.
// A schema that isn't a JSON object is replaced by an empty object schema;
// the catalog already refuses such a template.
func DeclaredTools(template *v1alpha1.MCPServerTemplate) []*mcp.Tool {
	tools := make([]*mcp.Tool, 0, len(template.Spec.DeclaredTools))

	for _, item := range template.Spec.DeclaredTools {
		tool := &mcp.Tool{Name: item.Name, Description: item.Description, InputSchema: map[string]any{"type": "object"}}

		if item.InputSchema != nil && len(item.InputSchema.Raw) > 0 {
			var schema map[string]any
			if json.Unmarshal(item.InputSchema.Raw, &schema) == nil && schema != nil {
				tool.InputSchema = schema
			}
		}

		tools = append(tools, tool)
	}

	return tools
}

// CatalogList returns the upstream tools of the template's catalog list, and
// whether the template has one: the first of its declared tools, the learned
// list of its current generation, and the learned list of the newest older
// generation (docs/design/phase-4.md §3). Learned holds the stored lists in
// any order. Curate the result before showing it.
func CatalogList(template *v1alpha1.MCPServerTemplate, learned []Learned) ([]*mcp.Tool, bool, error) {
	if len(template.Spec.DeclaredTools) > 0 {
		return DeclaredTools(template), true, nil
	}

	var best *Learned

	for index := range learned {
		candidate := &learned[index]
		if candidate.Generation > template.Generation {
			continue
		}

		if best == nil || candidate.Generation > best.Generation {
			best = candidate
		}
	}

	if best == nil {
		return nil, false, nil
	}

	var tools []*mcp.Tool
	if err := json.Unmarshal(best.Tools, &tools); err != nil {
		return nil, false, fmt.Errorf("decode the learned tools of generation %d: %w", best.Generation, err)
	}

	return tools, true, nil
}
