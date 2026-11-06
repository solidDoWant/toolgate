package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestRevisionModes(t *testing.T) {
	tests := []struct {
		mode      string
		probed    string
		stateless bool
	}{
		{mode: mode2025Only, probed: runtime.RevisionStateful},
		{mode: modeBoth, probed: runtime.RevisionStateful, stateless: true},
		{mode: mode2026Only, probed: runtime.RevisionStateless, stateless: true},
	}

	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			server := startHandler(t, handlerConfig{Mode: test.mode, Tools: "alpha"})

			session, revision, err := runtime.ConnectUpstream(t.Context(), server.URL+"/mcp", "", runtime.UpstreamOptions{})
			require.NoError(t, err)

			_ = session.Close()

			assert.Equal(t, test.probed, revision, "the probe's choice")

			session, revision, err = runtime.ConnectUpstream(t.Context(), server.URL+"/mcp", runtime.RevisionStateless, runtime.UpstreamOptions{})
			if test.stateless {
				require.NoError(t, err, "the server speaks 2026-07-28")

				_ = session.Close()

				assert.Equal(t, runtime.RevisionStateless, revision)
			} else {
				require.Error(t, err, "the server doesn't speak 2026-07-28")
			}

			changed := make(chan struct{}, 1)
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
				ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
					select {
					case changed <- struct{}{}:
					default:
					}
				},
			})

			listening, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"},
				&mcp.ClientSessionOptions{ProtocolVersion: test.probed})
			require.NoError(t, err)

			t.Cleanup(func() { _ = listening.Close() })

			response := httpDo(t, http.MethodPut, server.URL+"/tools?names=alpha,beta", "")
			assert.Equal(t, http.StatusNoContent, response.StatusCode, "PUT /tools changes the list")

			select {
			case <-changed:
			case <-time.After(10 * time.Second):
				t.Fatal("a session on the probed revision got no list_changed")
			}
		})
	}
}
