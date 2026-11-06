package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startHandler(t *testing.T, config handlerConfig) *httptest.Server {
	t.Helper()

	if config.Health == "" {
		config.Health = "200"
	}

	mux, err := newHandler(config)
	require.NoError(t, err)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func connectHTTP(t *testing.T, server *httptest.Server) (*mcp.ClientSession, error) {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}

	return session, err
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	var names []string

	for tool, err := range session.Tools(t.Context(), nil) {
		require.NoError(t, err)

		names = append(names, tool.Name)
	}

	return names
}

func httpDo(t *testing.T, method, target, body string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

func getCalls(t *testing.T, server *httptest.Server) map[string]int {
	t.Helper()

	response := httpDo(t, http.MethodGet, server.URL+"/calls", "")
	require.Equal(t, http.StatusOK, response.StatusCode)

	counts := map[string]int{}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&counts))

	return counts
}

func writeToolsFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tools.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

const fixedTools = `{"tools":[
  {"name":"search_docs","description":"Searches docs.","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},
  {"name":"list_issues"}
]}`

func TestToolsFileServesDeclaredToolsAndCountsCalls(t *testing.T) {
	server := startHandler(t, handlerConfig{ToolsFile: writeToolsFile(t, fixedTools)})

	session, err := connectHTTP(t, server)
	require.NoError(t, err)

	var found *mcp.Tool

	for tool, err := range session.Tools(t.Context(), nil) {
		require.NoError(t, err)

		if tool.Name == "search_docs" {
			found = tool
		}
	}

	require.NotNil(t, found)
	assert.Equal(t, "Searches docs.", found.Description)
	assert.ElementsMatch(t, []string{"search_docs", "list_issues"}, toolNames(t, session))
	assert.Empty(t, getCalls(t, server))

	var result echoResult

	callTool(t, session, "search_docs", map[string]any{"query": "x"}, &result)
	assert.Equal(t, echoResult{Tool: "search_docs", Arguments: map[string]any{"query": "x"}}, result)

	callTool(t, session, "search_docs", map[string]any{"query": "y"}, &result)
	callTool(t, session, "list_issues", nil, &result)

	assert.Equal(t, map[string]int{"search_docs": 2, "list_issues": 1}, getCalls(t, server))
}

func TestDefaultModeCountsCalls(t *testing.T) {
	server := startHandler(t, handlerConfig{Tools: "read_file,env"})

	session, err := connectHTTP(t, server)
	require.NoError(t, err)

	var echo echoResult

	callTool(t, session, "read_file", nil, &echo)
	callTool(t, session, "read_file", nil, &echo)

	var environment map[string]string

	callTool(t, session, "env", nil, &environment)

	assert.Equal(t, map[string]int{"read_file": 2, "env": 1}, getCalls(t, server))

	assert.Equal(t, http.StatusNotFound, httpDo(t, http.MethodPut, server.URL+"/tools?names=a", "").StatusCode,
		"the default mode has a fixed tool list")
}

func TestToolListCanBeReplaced(t *testing.T) {
	server := startHandler(t, handlerConfig{ToolsFile: writeToolsFile(t, fixedTools)})

	changed := make(chan struct{}, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) { changed <- struct{}{} },
	})

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	require.ElementsMatch(t, []string{"search_docs", "list_issues"}, toolNames(t, session))

	response := httpDo(t, http.MethodPut, server.URL+"/tools", `[{"name":"list_issues"},{"name":"new_tool"}]`)
	require.Equal(t, http.StatusNoContent, response.StatusCode)

	<-changed
	assert.ElementsMatch(t, []string{"list_issues", "new_tool"}, toolNames(t, session))

	response = httpDo(t, http.MethodPut, server.URL+"/tools?names=only_one", "")
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	assert.Equal(t, []string{"only_one"}, toolNames(t, session))

	for _, body := range []string{`not json`, `[{"name":"a"},{"name":"a"}]`, `[{"name":"a","inputSchema":{"type":"string"}}]`} {
		response = httpDo(t, http.MethodPut, server.URL+"/tools", body)
		assert.Equal(t, http.StatusBadRequest, response.StatusCode, body)
	}

	assert.Equal(t, []string{"only_one"}, toolNames(t, session), "a refused list changes nothing")
}

func TestTwentySixOnlyModeRefusesOtherRevisions(t *testing.T) {
	server := startHandler(t, handlerConfig{Mode: mode2026Only, Tools: "alpha,beta"})

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-11-25")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	assert.Contains(t, string(raw), "supported versions: 2026-07-28")
	assert.Empty(t, response.Header.Get("Mcp-Session-Id"), "the server is stateless")

	assert.Equal(t, http.StatusMethodNotAllowed, httpDo(t, http.MethodGet, server.URL+"/mcp", "").StatusCode,
		"a stateless server has no GET stream")
}

func TestTwentySixOnlyModeToolListChange(t *testing.T) {
	server := startHandler(t, handlerConfig{Mode: mode2026Only, Tools: "alpha,beta"})

	response := httpDo(t, http.MethodPut, server.URL+"/tools?names=beta,gamma", "")
	require.Equal(t, http.StatusNoContent, response.StatusCode)

	session, err := connectHTTP(t, server)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"beta", "gamma"}, toolNames(t, session))
}

func TestToolsFileIsValidated(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "not json", content: "nope"},
		{name: "duplicate", content: `[{"name":"a"},{"name":"a"}]`},
		{name: "empty name", content: `[{"name":""}]`},
		{name: "schema is not an object", content: `[{"name":"a","inputSchema":{"type":"array"}}]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newHandler(handlerConfig{ToolsFile: writeToolsFile(t, test.content), Health: "200"})
			require.Error(t, err)
		})
	}
}
