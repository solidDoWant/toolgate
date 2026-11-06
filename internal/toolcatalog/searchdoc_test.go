package toolcatalog_test

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

func TestSplitName(t *testing.T) {
	for input, want := range map[string]string{
		"createIssue":   "create Issue",
		"create_issue":  "create issue",
		"list-pull.all": "list pull all",
		"getHTTPThing":  "get HTTPThing",
		"plain":         "plain",
	} {
		assert.Equal(t, want, toolcatalog.SplitName(input), input)
	}
}

func TestSearchPartsOf(t *testing.T) {
	template := &v1alpha1.MCPServerTemplate{}
	template.Name = "github"
	template.Spec.Description = "Code hosting"

	tool := toolcatalog.Tool{ExposedName: "createIssue", Tool: &mcp.Tool{
		Name: "createIssue", Description: "Open an issue",
		InputSchema: map[string]any{"properties": map[string]any{
			"repoName": map[string]any{"description": "The repository"},
			"title":    map[string]any{"type": "string"},
		}},
	}}

	parts := toolcatalog.SearchPartsOf(template, tool)
	assert.Equal(t, "create Issue", parts.Name)
	assert.Equal(t, "Open an issue", parts.Description)
	assert.Equal(t, "github Code hosting", parts.Server)
	assert.Equal(t, "repo Name The repository title", parts.Input)
}

func TestSearchPartsCutAtFourKiB(t *testing.T) {
	template := &v1alpha1.MCPServerTemplate{}

	for size, want := range map[int]int{4096: 4096, 4097: 4096} {
		tool := toolcatalog.Tool{Tool: &mcp.Tool{Description: strings.Repeat("a", size)}}
		assert.Len(t, toolcatalog.SearchPartsOf(template, tool).Description, want, "size %d", size)
	}

	// A cut never splits a character: "é" is two bytes.
	tool := toolcatalog.Tool{Tool: &mcp.Tool{Description: strings.Repeat("a", 4095) + "é"}}
	assert.Len(t, toolcatalog.SearchPartsOf(template, tool).Description, 4095)

	long := strings.Repeat("d", 5000)
	tool = toolcatalog.Tool{Tool: &mcp.Tool{InputSchema: map[string]any{"properties": map[string]any{
		"x": map[string]any{"description": long},
	}}}}
	assert.Len(t, toolcatalog.SearchPartsOf(template, tool).Input, 4096)
}
