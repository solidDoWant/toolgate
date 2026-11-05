package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connect starts the server with options and returns a connected client
// session.
func connect(t *testing.T, options options) *mcp.ClientSession {
	t.Helper()

	server, err := newServer(options)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any, result any) {
	t.Helper()

	response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	require.False(t, response.IsError, "tool %s failed: %v", name, response.Content)

	encoded, err := json.Marshal(response.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, result))
}

func TestToolsAreConfigurable(t *testing.T) {
	tests := []struct {
		name      string
		tools     []string
		wantTools []string
		wantErr   require.ErrorAssertionFunc
	}{
		{name: "env and secrets", tools: []string{"env", "secrets"}, wantTools: []string{"env", "secrets"}},
		{name: "chosen names", tools: []string{"read_file", "env", "write_file"}, wantTools: []string{"env", "read_file", "write_file"}},
		{name: "none", tools: nil, wantTools: nil},
		{name: "duplicate", tools: []string{"echo", "echo"}, wantErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.wantErr != nil {
				_, err := newServer(options{Tools: test.tools})
				test.wantErr(t, err)

				return
			}

			session := connect(t, options{Tools: test.tools, Environ: os.Environ})

			listed, err := session.ListTools(t.Context(), nil)
			if len(test.wantTools) == 0 {
				// A server without tools does not advertise the capability.
				if err == nil {
					assert.Empty(t, listed.Tools)
				}

				return
			}

			require.NoError(t, err)

			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}

			assert.ElementsMatch(t, test.wantTools, names)
		})
	}
}

func TestEnvToolEchoesEnvironment(t *testing.T) {
	session := connect(t, options{
		Tools:   []string{"env"},
		Environ: func() []string { return []string{"GREETING=hello=world", "EMPTY="} },
	})

	var environment map[string]string
	callTool(t, session, "env", nil, &environment)

	assert.Equal(t, map[string]string{"GREETING": "hello=world", "EMPTY": ""}, environment)
}

func TestSecretsToolEchoesMountedSecret(t *testing.T) {
	// Lay the directory out like a mounted Kubernetes Secret: the keys are
	// links into a hidden timestamped directory.
	dir := t.TempDir()
	data := filepath.Join(dir, "..2026_11_05")
	require.NoError(t, os.Mkdir(data, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(data, "token"), []byte("s3cret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(data, "user"), []byte("alice"), 0o600))
	require.NoError(t, os.Symlink("..2026_11_05", filepath.Join(dir, "..data")))
	require.NoError(t, os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")))
	require.NoError(t, os.Symlink(filepath.Join("..data", "user"), filepath.Join(dir, "user")))

	session := connect(t, options{Tools: []string{"secrets"}, SecretsDir: dir})

	var files map[string]string
	callTool(t, session, "secrets", nil, &files)

	assert.Equal(t, map[string]string{"token": "s3cret", "user": "alice"}, files)
}

func TestSecretsToolWithoutMount(t *testing.T) {
	session := connect(t, options{Tools: []string{"secrets"}, SecretsDir: filepath.Join(t.TempDir(), "missing")})

	var files map[string]string
	callTool(t, session, "secrets", nil, &files)

	assert.Empty(t, files)
}

func TestOtherToolsEchoArguments(t *testing.T) {
	session := connect(t, options{Tools: []string{"read_file"}})

	var result echoResult
	callTool(t, session, "read_file", map[string]any{"path": "/etc/hosts"}, &result)

	assert.Equal(t, echoResult{Tool: "read_file", Arguments: map[string]any{"path": "/etc/hosts"}}, result)
}

func TestParseTools(t *testing.T) {
	assert.Equal(t, []string{"env", "secrets", "read_file"}, parseTools(" env, secrets,,read_file "))
	assert.Empty(t, parseTools(""))
}
