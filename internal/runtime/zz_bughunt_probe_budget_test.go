package runtime_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
)

// The probe lists the tools inside ConnectUpstream and MCPToolLister lists
// them again, both against one response budget. A tool list whose response
// fits the budget once, and whose stored (re-encoded) form is under 1 MiB,
// is then refused as too large. Here the upstream escapes non-ASCII text as
// \uXXXX, as Python's json.dumps does by default: 6 bytes on the wire per
// character that Go stores in 3.
func TestBughuntProbeListsToolsTwiceAgainstOneBudget(t *testing.T) {
	t.Parallel()

	const tools = 100

	description := strings.Repeat(`\u65e5`, 2200) // 13.2 KB on the wire, 6.6 KB stored

	var list strings.Builder
	for index := range tools {
		if index > 0 {
			list.WriteString(",")
		}

		fmt.Fprintf(&list, `{"name":"tool%d","description":"%s","inputSchema":{"type":"object"}}`, index, description)
	}

	endpoint, _ := scriptedUpstream(t, func(w http.ResponseWriter, request *jsonrpc.Request) {
		switch request.Method {
		case "initialize":
			initializeResult(w, request, runtime.RevisionStateful)
		case "tools/list":
			writeResponse(w, &jsonrpc.Response{ID: request.ID, Result: []byte(`{"tools":[` + list.String() + `]}`)})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	t.Logf("tools/list response is about %d bytes", list.Len())

	// Control: one listing (the 0.3 lister) against the same budget fits.
	controlCtx, _ := upstreamlimit.WithBudget(t.Context(), runtime.MaxToolListBytes+upstreamlimit.Slack)
	client := mcp.NewClient(&mcp.Implementation{Name: "control", Version: "0"}, nil)
	controlSession, err := client.Connect(controlCtx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true,
		HTTPClient: upstreamlimit.NewClient(http.DefaultClient, runtime.MaxToolListBytes+upstreamlimit.Slack),
	}, &mcp.ClientSessionOptions{ProtocolVersion: runtime.RevisionStateful})
	require.NoError(t, err)

	controlCount := 0
	for _, listErr := range controlSession.Tools(controlCtx, nil) {
		require.NoError(t, listErr, "control: one listing fits the budget")
		controlCount++
	}
	_ = controlSession.Close()
	require.Equal(t, tools, controlCount)

	listed, revision, err := runtime.MCPToolLister(t.Context(), endpoint)
	require.NoError(t, err, "a list that fits the budget once must not be refused")
	assert.Equal(t, runtime.RevisionStateful, revision)
	assert.Len(t, listed, tools)

	_, reason := runtime.CheckCuratedToolList(&v1alpha1.MCPServerTemplate{}, listed)
	assert.Empty(t, reason, "the stored list is within 1 MiB")
}
