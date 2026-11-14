package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolsFileSize bounds the tool list file and the body of a tool list
// change.
const maxToolsFileSize = 4 << 20

// onlyProtocolVersion is the one protocol version of the 2026-only mode.
const onlyProtocolVersion = "2026-07-28"

// toolSpec declares one tool of a fixed tool list. A call to it echoes its
// arguments, as the tools of the default mode do.
type toolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema of the arguments. Default: an object
	// with no properties.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// parseToolSpecs reads a tool list: a JSON array of tool specs, or an object
// with the array in its "tools" field.
func parseToolSpecs(data []byte) ([]toolSpec, error) {
	var specs []toolSpec

	if err := json.Unmarshal(data, &specs); err != nil {
		var wrapped struct {
			Tools []toolSpec `json:"tools"`
		}

		if wrapErr := json.Unmarshal(data, &wrapped); wrapErr != nil {
			return nil, fmt.Errorf("a tool list is a JSON array of tools, or an object with a tools array: %w", err)
		}

		specs = wrapped.Tools
	}

	return specs, validateSpecs(specs)
}

// validateSpecs checks that the names of specs are unique and not empty.
func validateSpecs(specs []toolSpec) error {
	seen := map[string]bool{}

	for _, spec := range specs {
		if spec.Name == "" || seen[spec.Name] {
			return fmt.Errorf("tool names must be unique and not empty: %q", spec.Name)
		}

		seen[spec.Name] = true
	}

	return nil
}

// readToolSpecs reads the tool list file at path.
func readToolSpecs(path string) ([]toolSpec, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if info.Size() > maxToolsFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxToolsFileSize)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return parseToolSpecs(data)
}

// callCounter counts the calls to each tool.
type callCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *callCounter) add(tool string) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts == nil {
		c.counts = map[string]int{}
	}

	c.counts[tool]++
}

// snapshot returns a copy of the counts.
func (c *callCounter) snapshot() map[string]int {
	counts := map[string]int{}

	if c == nil {
		return counts
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for tool, count := range c.counts {
		counts[tool] = count
	}

	return counts
}

// toolSet is the tools of a server whose list a test can replace. Every
// replacement notifies the connected sessions of the list change.
type toolSet struct {
	server *mcp.Server
	calls  *callCounter

	mu    sync.Mutex
	names []string
}

// set replaces the server's tools with specs.
func (s *toolSet) set(specs []toolSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make([]string, 0, len(specs))

	for _, spec := range specs {
		tool, err := spec.tool()
		if err != nil {
			return err
		}

		s.server.AddTool(tool, s.handler(spec.Name))

		next = append(next, spec.Name)
	}

	var removed []string

	for _, name := range s.names {
		if !slices.Contains(next, name) {
			removed = append(removed, name)
		}
	}

	if len(removed) > 0 {
		s.server.RemoveTools(removed...)
	}

	s.names = next

	return nil
}

// tool builds the MCP tool for the spec.
func (t toolSpec) tool() (*mcp.Tool, error) {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}

	var parsed map[string]any

	if err := json.Unmarshal(schema, &parsed); err != nil {
		return nil, fmt.Errorf("tool %q: inputSchema: %w", t.Name, err)
	}

	if parsed["type"] != "object" {
		return nil, fmt.Errorf("tool %q: inputSchema must have type object", t.Name)
	}

	return &mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: parsed}, nil
}

// textResult is a result with one text content.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// countSessions returns how many sessions the server holds.
func countSessions(server *mcp.Server) int {
	count := 0

	for range server.Sessions() {
		count++
	}

	return count
}

// handler counts the call and echoes the arguments.
func (s *toolSet) handler(name string) mcp.ToolHandler {
	return func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.calls.add(name)

		switch name {
		case sessionIDTool:
			return textResult(request.Session.ID()), nil
		case liveSessionsTool:
			return textResult(strconv.Itoa(countSessions(s.server))), nil
		}

		arguments := map[string]any{}

		if len(request.Params.Arguments) > 0 {
			if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
				return nil, err
			}
		}

		result := echoResult{Tool: name, Arguments: arguments}

		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}

		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
			StructuredContent: result,
		}, nil
	}
}
