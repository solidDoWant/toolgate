package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names with their own behaviour. Any other configured name echoes its
// arguments.
const (
	envTool     = "env"
	secretsTool = "secrets"
	// whoamiTool returns the user whose bearer token the request carried.
	whoamiTool = "whoami"
	// countTool is read-only and returns how often it was called, so a test
	// sees whether a call reached the server or was served from a cache.
	countTool = "count"
	// sessionIDTool returns the Mcp-Session-Id of the request's session.
	sessionIDTool = "session_id"
	// liveSessionsTool returns how many sessions the server holds.
	liveSessionsTool = "live_sessions"
	// writeNoteTool writes its text argument to the note file in the data
	// directory, and readNoteTool returns the note, or an empty text when
	// there is none.
	writeNoteTool = "write_note"
	readNoteTool  = "read_note"
)

// noteFileName is the file in the data directory that holds the note.
const noteFileName = "note.txt"

// noteArguments are the arguments of write_note.
type noteArguments struct {
	Text string `json:"text"`
}

// maxSecretFileSize bounds each file the secrets tool reads.
const maxSecretFileSize = 1 << 20

// options configures the test MCP server.
type options struct {
	// Tools are the names of the tools the server exposes, in order.
	Tools []string
	// SecretsDir is the directory the secrets tool reads.
	SecretsDir string
	// DataDir is the directory the note tools write to and read from.
	DataDir string
	// Environ returns the environment the env tool reports.
	Environ func() []string
	// Calls counts the calls to each tool. Optional.
	Calls *callCounter
	// ProtocolVersions, when set, restricts the protocol versions the
	// server speaks.
	ProtocolVersions []string
	// Users maps a bearer token to its user, for the whoami tool.
	Users map[string]string
	// CountTTL, when set, is the count tool's cache lifetime suggestion,
	// `_meta["toolgate/cache"].ttl` in its tool list entry.
	CountTTL string
}

// countResult is the structured result of the count tool.
type countResult struct {
	Calls int `json:"calls"`
}

// echoResult is the structured result of an echo tool.
type echoResult struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// newServer returns an MCP server that exposes options.Tools.
func newServer(options options) (*mcp.Server, error) {
	server := newBareServer(options.ProtocolVersions)

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
					options.Calls.add(name)

					return nil, environment(options.Environ()), nil
				})
		case secretsTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the files in the mounted secrets directory, by file name."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
					options.Calls.add(name)

					files, err := readSecrets(options.SecretsDir)

					return nil, files, err
				})
		case whoamiTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the user of the request's bearer token."},
				func(_ context.Context, request *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]string, error) {
					options.Calls.add(name)

					var token string
					if extra := request.GetExtra(); extra != nil {
						token = strings.TrimPrefix(extra.Header.Get("Authorization"), bearerPrefix)
					}

					return nil, map[string]string{"user": options.Users[token]}, nil
				})
		case countTool:
			tool := &mcp.Tool{
				Name: name, Description: "Returns how often it was called.",
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
			}
			if options.CountTTL != "" {
				tool.Meta = mcp.Meta{"toolgate/cache": map[string]any{"ttl": options.CountTTL}}
			}

			var (
				mu    sync.Mutex
				calls int
			)

			mcp.AddTool(server, tool,
				func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, countResult, error) {
					options.Calls.add(name)

					mu.Lock()
					defer mu.Unlock()

					calls++

					return nil, countResult{Calls: calls}, nil
				})
		case sessionIDTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the ID of the request's session."},
				func(_ context.Context, request *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
					options.Calls.add(name)

					return textResult(request.Session.ID()), nil, nil
				})
		case liveSessionsTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns how many sessions the server holds."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					options.Calls.add(name)

					return textResult(strconv.Itoa(countSessions(server))), nil, nil
				})
		case writeNoteTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Writes the text to the note file in the data directory."},
				func(_ context.Context, _ *mcp.CallToolRequest, arguments noteArguments) (*mcp.CallToolResult, any, error) {
					options.Calls.add(name)

					if err := writeNote(options.DataDir, arguments.Text); err != nil {
						return nil, nil, err
					}

					return textResult("ok"), nil, nil
				})
		case readNoteTool:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Returns the note in the data directory, or an empty text."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					options.Calls.add(name)

					text, err := readNote(options.DataDir)
					if err != nil {
						return nil, nil, err
					}

					return textResult(text), nil, nil
				})
		default:
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Echoes its arguments."},
				func(_ context.Context, _ *mcp.CallToolRequest, arguments map[string]any) (*mcp.CallToolResult, echoResult, error) {
					options.Calls.add(name)

					if arguments == nil {
						arguments = map[string]any{}
					}

					return nil, echoResult{Tool: name, Arguments: arguments}, nil
				})
		}
	}

	return server, nil
}

// newBareServer returns a server with no tools. A non-empty versions
// restricts the protocol versions it speaks.
func newBareServer(versions []string) *mcp.Server {
	var serverOptions *mcp.ServerOptions

	if len(versions) > 0 {
		serverOptions = &mcp.ServerOptions{SupportedProtocolVersions: versions}
	}

	return mcp.NewServer(&mcp.Implementation{Name: "toolgate-test-mcpserver", Version: "0.0.0"}, serverOptions)
}

// newToolListServer returns a server whose tools are specs, and the set
// that replaces them.
func newToolListServer(specs []toolSpec, calls *callCounter, versions []string) (*mcp.Server, *toolSet, error) {
	server := newBareServer(versions)
	tools := &toolSet{server: server, calls: calls}

	if err := tools.set(specs); err != nil {
		return nil, nil, err
	}

	return server, tools, nil
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

// writeNote replaces the note in dir with text.
func writeNote(dir, text string) error {
	if dir == "" {
		return errors.New("the server has no data directory")
	}

	return os.WriteFile(filepath.Join(dir, noteFileName), []byte(text), 0o600)
}

// readNote returns the note in dir, or an empty text when there is none.
func readNote(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("the server has no data directory")
	}

	data, err := os.ReadFile(filepath.Join(dir, noteFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	return string(data), err
}
