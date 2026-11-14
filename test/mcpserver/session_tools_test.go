package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func callText(t *testing.T, session *mcp.ClientSession, name string) string {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name})
	require.NoError(t, err)
	require.False(t, result.IsError, "tool %s failed: %v", name, result.Content)
	require.Len(t, result.Content, 1)

	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	return text.Text
}

func TestSessionToolsReportSessionIDAndLiveSessions(t *testing.T) {
	// Both ways of building the server: a configured tool list, and the
	// default mode with named tools.
	tests := []struct {
		name   string
		config handlerConfig
	}{
		{name: "default mode", config: handlerConfig{Tools: "session_id,live_sessions"}},
		{name: "tool list mode", config: handlerConfig{Mode: mode2025Only, Tools: "session_id,live_sessions"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startHandler(t, test.config)

			first, err := connectHTTP(t, server)
			require.NoError(t, err)

			id := callText(t, first, "session_id")
			assert.NotEmpty(t, id)
			assert.Equal(t, first.ID(), id, "the tool returns the Mcp-Session-Id of the session")
			assert.Equal(t, id, callText(t, first, "session_id"), "the ID is stable for a session")
			assert.Equal(t, "1", callText(t, first, "live_sessions"))

			second, err := connectHTTP(t, server)
			require.NoError(t, err)

			assert.NotEqual(t, id, callText(t, second, "session_id"))

			live, err := strconv.Atoi(strings.TrimSpace(callText(t, second, "live_sessions")))
			require.NoError(t, err)
			assert.Equal(t, 2, live)

			require.NoError(t, first.Close())

			assert.Equal(t, "1", callText(t, second, "live_sessions"), "a closed session is not counted")
		})
	}
}
