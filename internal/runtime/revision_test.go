package runtime_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// methodLog records the JSON-RPC methods an upstream received, in order.
type methodLog struct {
	mu      sync.Mutex
	methods []string
}

func (l *methodLog) add(method string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.methods = append(l.methods, method)
}

func (l *methodLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.methods...)
}

// scriptedUpstream answers each JSON-RPC POST with answer, and records the
// methods it received.
func scriptedUpstream(t *testing.T, answer func(w http.ResponseWriter, request *jsonrpc.Request)) (string, *methodLog) {
	t.Helper()

	log := &methodLog{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		message, err := jsonrpc.DecodeMessage(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		request, ok := message.(*jsonrpc.Request)
		if !ok {
			w.WriteHeader(http.StatusAccepted)

			return
		}

		log.add(request.Method)
		answer(w, request)
	}))
	t.Cleanup(server.Close)

	return server.URL, log
}

// upstreamOn serves a go-sdk server limited to versions, stateless when
// stateless is set, with one tool.
func upstreamOn(t *testing.T, versions []string, stateless bool) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"},
		&mcp.ServerOptions{SupportedProtocolVersions: versions})
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: stateless})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

func TestConnectUpstreamChoosesTheRevision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint func(t *testing.T) string
		want     string
	}{
		{
			name:     "2025-11-25 only",
			endpoint: func(t *testing.T) string { return upstreamOn(t, []string{runtime.RevisionStateful}, false) },
			want:     runtime.RevisionStateful,
		},
		{
			name:     "both revisions",
			endpoint: func(t *testing.T) string { return upstreamOn(t, nil, false) },
			want:     runtime.RevisionStateful,
		},
		{
			name:     "2026-07-28 only",
			endpoint: func(t *testing.T) string { return upstreamOn(t, []string{runtime.RevisionStateless}, true) },
			want:     runtime.RevisionStateless,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session, revision, err := runtime.ConnectUpstream(t.Context(), test.endpoint(t), "", runtime.UpstreamOptions{})
			require.NoError(t, err)

			t.Cleanup(func() { _ = session.Close() })

			assert.Equal(t, test.want, revision)

			listed, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			assert.Len(t, listed.Tools, 1)
		})
	}
}

func TestProbeTreatsA503AsNoVersionAnswer(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			endpoint, log := scriptedUpstream(t, func(w http.ResponseWriter, _ *jsonrpc.Request) {
				w.WriteHeader(status)
			})

			session, revision, err := runtime.ConnectUpstream(t.Context(), endpoint, "", runtime.UpstreamOptions{})
			require.Error(t, err)
			assert.Nil(t, session)
			assert.Empty(t, revision, "no revision is stored")
			assert.NotContains(t, log.all(), "server/discover", "no 2026-07-28 attempt is made")
			assert.NotErrorIs(t, err, runtime.ErrTransientVersionAnswer)
		})
	}
}

func TestProbeTreatsAnOlderDiscoverListAsTransient(t *testing.T) {
	t.Parallel()

	endpoint, log := scriptedUpstream(t, func(w http.ResponseWriter, request *jsonrpc.Request) {
		switch request.Method {
		case "server/discover":
			result, err := json.Marshal(map[string]any{
				"supportedVersions": []string{runtime.RevisionStateful},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			encoded, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: request.ID, Result: result})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(encoded)
		default:
			// A version answer: a plain-text 400 to initialize.
			http.Error(w, "Bad Request: Unsupported protocol version", http.StatusBadRequest)
		}
	})

	session, revision, err := runtime.ConnectUpstream(t.Context(), endpoint, "", runtime.UpstreamOptions{})
	require.ErrorIs(t, err, runtime.ErrTransientVersionAnswer)
	assert.Nil(t, session)
	assert.Empty(t, revision, "no revision is stored")
	assert.Equal(t, []string{"initialize", "server/discover"}, log.all(),
		"step 2 sends server/discover and never falls back to initialize")

	_, _, err = runtime.ConnectUpstream(t.Context(), endpoint, "", runtime.UpstreamOptions{})
	require.ErrorIs(t, err, runtime.ErrTransientVersionAnswer)
	assert.Equal(t, []string{"initialize", "server/discover", "initialize", "server/discover"}, log.all(),
		"the next attempt starts again from step 1")
}

// initializeResult answers initialize with a result on revision.
func initializeResult(w http.ResponseWriter, request *jsonrpc.Request, revision string) {
	result, err := json.Marshal(map[string]any{
		"protocolVersion": revision, "capabilities": map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{"name": "scripted", "version": "1"},
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	writeResponse(w, &jsonrpc.Response{ID: request.ID, Result: result})
}

func writeResponse(w http.ResponseWriter, response *jsonrpc.Response) {
	encoded, err := jsonrpc.EncodeMessage(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

func TestConnectUpstreamTriesTheNewerRevisionOnlyAfterAVersionAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer func(w http.ResponseWriter, request *jsonrpc.Request)
	}{
		{
			name: "HTTP 400 to initialize",
			answer: func(w http.ResponseWriter, _ *jsonrpc.Request) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
			},
		},
		{
			name: "HTTP 404 to initialize",
			answer: func(w http.ResponseWriter, _ *jsonrpc.Request) {
				http.Error(w, "Not Found", http.StatusNotFound)
			},
		},
		{
			name: "HTTP 405 to initialize",
			answer: func(w http.ResponseWriter, _ *jsonrpc.Request) {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			},
		},
		{
			name: "JSON-RPC error to initialize",
			answer: func(w http.ResponseWriter, request *jsonrpc.Request) {
				writeResponse(w, &jsonrpc.Response{ID: request.ID, Error: &jsonrpc.Error{Code: -32601, Message: "method not found"}})
			},
		},
		{
			name: "initialize result with a refused revision",
			answer: func(w http.ResponseWriter, request *jsonrpc.Request) {
				initializeResult(w, request, "1999-01-01")
			},
		},
		{
			name: "HTTP 400 to the first request after initialize",
			answer: func(w http.ResponseWriter, request *jsonrpc.Request) {
				if request.Method == "initialize" {
					initializeResult(w, request, runtime.RevisionStateful)

					return
				}

				http.Error(w, "Bad Request", http.StatusBadRequest)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			endpoint, log := scriptedUpstream(t, func(w http.ResponseWriter, request *jsonrpc.Request) {
				if request.Method == "server/discover" {
					w.WriteHeader(http.StatusServiceUnavailable)

					return
				}

				test.answer(w, request)
			})

			_, revision, err := runtime.ConnectUpstream(t.Context(), endpoint, "", runtime.UpstreamOptions{})
			require.Error(t, err, "the scripted server never serves a session")
			assert.Empty(t, revision)
			assert.Contains(t, log.all(), "server/discover", "a version answer leads to a 2026-07-28 attempt")
		})
	}
}
