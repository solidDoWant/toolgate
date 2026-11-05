package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names with their own behaviour. Any other configured name echoes its
// arguments.
const (
	envTool     = "env"
	secretsTool = "secrets"
)

// maxSecretFileSize bounds each file the secrets tool reads.
const maxSecretFileSize = 1 << 20

// options configures the test MCP server.
type options struct {
	// Tools are the names of the tools the server exposes, in order.
	Tools []string
	// SecretsDir is the directory the secrets tool reads.
	SecretsDir string
	// Environ returns the environment the env tool reports.
	Environ func() []string
}

// echoResult is the structured result of an echo tool.
type echoResult struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// newServer returns an MCP server that exposes options.Tools.
func newServer(options options) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "toolgate-test-mcpserver", Version: "0.0.0"}, nil)

	seen := map[string]bool{}

	for _, name := range options.Tools {
		if name == "" || seen[name] {
			return nil, fmt.Errorf("tool names must be unique and not empty: %q", name)
		}

		seen[name] = true

		switch name {
		case envTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the server's environment variables."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
					return nil, environment(options.Environ()), nil
				})
		case secretsTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the files in the mounted secrets directory, by file name."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
					files, err := readSecrets(options.SecretsDir)

					return nil, files, err
				})
		default:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Echoes its arguments."},
				func(_ context.Context, _ *mcp.CallToolRequest, arguments map[string]any) (*mcp.CallToolResult, echoResult, error) {
					if arguments == nil {
						arguments = map[string]any{}
					}

					return nil, echoResult{Tool: name, Arguments: arguments}, nil
				})
		}
	}

	return server, nil
}

// environment turns KEY=value entries into a map.
func environment(entries []string) map[string]string {
	values := make(map[string]string, len(entries))

	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}

	return values
}

// readSecrets returns the regular files in dir by name. It follows the
// symbolic links of a mounted Kubernetes Secret and skips directories (the
// mount's ..data entries). A missing directory gives no files.
func readSecrets(dir string) (map[string]string, error) {
	files := map[string]string{}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}

	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())

		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}

		if !info.Mode().IsRegular() {
			continue
		}

		if info.Size() > maxSecretFileSize {
			return nil, fmt.Errorf("%s is larger than %d bytes", entry.Name(), maxSecretFileSize)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		files[entry.Name()] = string(content)
	}

	return files, nil
}

// parseTools splits a comma-separated list of tool names.
func parseTools(list string) []string {
	var names []string

	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}

	return slices.Clip(names)
}
