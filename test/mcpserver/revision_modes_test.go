package main

import (
	"net/http"
	"testing"

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

			response := httpDo(t, http.MethodPut, server.URL+"/tools?names=alpha,beta", "")
			assert.Equal(t, http.StatusNoContent, response.StatusCode, "PUT /tools changes the list")
		})
	}
}
