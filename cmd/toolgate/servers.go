package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/version"
)

// servers runs `toolgate servers`: it calls toolgate_list_servers as the
// signed-in user and prints the result. It returns the exit code.
func servers(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate servers: unexpected argument %q\n", args[0])

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	result, err := listServers(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate servers: %v\n", err)

		return 1
	}

	if len(result.Servers) == 0 {
		_, _ = fmt.Fprintln(stdout, "No servers are available to you.")

		return 0
	}

	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tSCOPE\tSTATUS\tENABLED\tDESCRIPTION")

	for _, server := range result.Servers {
		status := server.Status
		if server.Reason != "" {
			status += " (" + server.Reason + ")"
		}

		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%t\t%s\n", server.Name, server.Scope, status, server.Enabled, server.Description)
	}

	if err := table.Flush(); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate servers: %v\n", err)

		return 1
	}

	return 0
}

// identityTransport adds the access token and the identity headers to every
// request.
type identityTransport struct {
	header http.Header
	// refused is set when the gateway answers 401.
	refused *atomic.Bool
}

func (t identityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range t.header {
		request.Header[name] = values
	}

	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusUnauthorized {
		t.refused.Store(true)
	}

	return response, err
}

// listServers connects to the gateway's MCP endpoint and calls
// toolgate_list_servers. The session is TOOLGATE_SESSION when it is set, and
// otherwise a new one for this command; the workspace is the one
// `toolgate headers` would send from this directory.
func listServers(ctx context.Context) (gateway.ListServersResult, error) {
	var result gateway.ListServersResult

	err := withAccess(ctx, func(access gatewayAccess) error {
		var err error

		result, err = listServersWith(ctx, access)

		return err
	})

	return result, err
}

func listServersWith(ctx context.Context, access gatewayAccess) (gateway.ListServersResult, error) {
	session := os.Getenv(headers.SessionEnv)
	if session == "" {
		session = "cli-" + rand.Text()
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return gateway.ListServersResult{}, fmt.Errorf("read the working directory: %w", err)
	}

	workspace, err := headers.Workspace(ctx, workingDir)
	if err != nil {
		return gateway.ListServersResult{}, err
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+access.accessToken)
	header.Set(auth.SessionHeader, session)
	header.Set(auth.WorkspaceHeader, workspace)

	refused := &atomic.Bool{}

	client := mcp.NewClient(&mcp.Implementation{Name: "toolgate", Version: version.Get()}, nil)

	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   access.gateway + gateway.MCPPath,
		HTTPClient: &http.Client{Transport: identityTransport{header: header, refused: refused}},
	}, nil)
	if err != nil {
		if refused.Load() {
			err = errors.Join(errUnauthorized, err)
		}

		return gateway.ListServersResult{}, fmt.Errorf("connect to the gateway: %w", err)
	}

	defer func() { _ = clientSession.Close() }()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: gateway.ToolListServers})
	if err != nil {
		return gateway.ListServersResult{}, fmt.Errorf("call %s: %w", gateway.ToolListServers, err)
	}

	if result.IsError {
		return gateway.ListServersResult{}, fmt.Errorf("%s failed: %s", gateway.ToolListServers, toolText(result))
	}

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return gateway.ListServersResult{}, err
	}

	var decoded gateway.ListServersResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return gateway.ListServersResult{}, fmt.Errorf("read the gateway's answer: %w", err)
	}

	return decoded, nil
}

// toolText joins the text content of a tool result.
func toolText(result *mcp.CallToolResult) string {
	var text string

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}

	return text
}
