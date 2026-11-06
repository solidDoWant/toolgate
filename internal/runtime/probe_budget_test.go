package runtime_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// escapedToolListUpstream serves a tool list whose text is escaped as
// \uXXXX, as Python's json.dumps does by default: 6 bytes on the wire per
// character that is stored in 3.
func escapedToolListUpstream(t *testing.T, tools int) string {
	t.Helper()

	description := strings.Repeat(`\u65e5`, 2200) // 13.2 KB on the wire, 6.6 KB stored

	var list strings.Builder

	for index := range tools {
		if index > 0 {
			list.WriteString(",")
		}

		fmt.Fprintf(&list, `{"name":"tool%d","description":"%s","inputSchema":{"type":"object"}}`, index, description)
	}

	endpoint, _ := scriptedUpstream(t, func(w http.ResponseWriter, request *jsonrpc.Request) {
		id, _ := json.Marshal(request.ID.Raw())
		switch request.Method {
		case "initialize":
			initializeResult(w, request, runtime.RevisionStateful)
		case "tools/list":
			// Written as is: re-encoding the message would unescape the text.
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[%s]}}`, id, list.String())
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	return endpoint
}

// The probe lists tools once to pick the revision and the lister lists them
// again. Each read gets the whole budget, so a response that fits it once is
// accepted.
func TestMCPToolListerAcceptsListThatFitsBudgetOnce(t *testing.T) {
	t.Parallel()

	const tools = 100 // about 1.3 MB on the wire, 0.66 MB stored

	endpoint := escapedToolListUpstream(t, tools)

	listed, revision, err := runtime.MCPToolLister(t.Context(), endpoint)
	require.NoError(t, err)
	assert.Equal(t, runtime.RevisionStateful, revision)
	assert.Len(t, listed, tools)

	_, reason := runtime.CheckCuratedToolList(&v1alpha1.MCPServerTemplate{}, listed)
	assert.Empty(t, reason, "the stored list is within 1 MiB")
}

func TestMCPToolListerRefusesListOverBudget(t *testing.T) {
	t.Parallel()

	endpoint := escapedToolListUpstream(t, 200) // about 2.6 MB on the wire

	_, _, err := runtime.MCPToolLister(t.Context(), endpoint)
	require.ErrorIs(t, err, runtime.ErrToolListTooLarge)
}
