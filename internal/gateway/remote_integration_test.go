//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
)

// userSecrets is a secret store for the gateway's remote headers: values by
// principal and name.
type userSecrets struct {
	mu     sync.Mutex
	values map[string]map[string]string
}

func (s *userSecrets) source(principal string) runtime.SecretSource {
	return secretSource{store: s, principal: principal}
}

type secretSource struct {
	store     *userSecrets
	principal string
}

func (s secretSource) Secret(_ context.Context, name string) (string, time.Time, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	value, found := s.store.values[s.principal][name]
	if !found {
		return "", time.Time{}, secrets.ErrNotFound
	}

	return value, time.Now(), nil
}

// remoteFixture is a gateway with a remote server, its upstream and the
// users' secrets, and buffers of the gateway's logs and audit output.
type remoteFixture struct {
	*mcpFixture

	secrets  *userSecrets
	upstream *remoteUpstream
	logs     *lockedBuffer
	audit    *lockedBuffer
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// remoteUpstream is a remote MCP server that accepts only the bearer keys
// in accepted and answers 401 to any other. It records the Authorization
// header of each tool call and counts the sessions closed.
type remoteUpstream struct {
	mu       sync.Mutex
	accepted map[string]bool
	calls    []string
	sessions []string
	closed   int
}

// callSessions returns the upstream MCP session ID of each tool call.
func (u *remoteUpstream) callSessions() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]string(nil), u.sessions...)
}

func (u *remoteUpstream) callAuthorizations() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]string(nil), u.calls...)
}

func (u *remoteUpstream) closedSessions() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.closed
}

const remoteServer = "mail-archiver"

const remoteSecret = "mail-archiver-api-key"

func newRemoteFixture(t *testing.T, accepted ...string) *remoteFixture {
	t.Helper()

	return newRemoteFixtureWith(t, func(*gateway.MCPOptions) {}, accepted...)
}

// newRemoteFixtureWith is newRemoteFixture with more gateway options.
func newRemoteFixtureWith(t *testing.T, configure func(*gateway.MCPOptions), accepted ...string) *remoteFixture {
	t.Helper()

	fixture := &remoteFixture{
		secrets: &userSecrets{values: map[string]map[string]string{}},
		logs:    &lockedBuffer{},
		audit:   &lockedBuffer{},
	}

	fixture.mcpFixture = newMCPFixture(t, func(options *gateway.MCPOptions) {
		options.Secrets = fixture.secrets.source
		options.Logger = slog.New(slog.NewJSONHandler(fixture.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		options.AuditOutput = fixture.audit
		configure(options)
	})

	// The remote server starts on enable: it declares no tools.
	template := fixture.addRemoteTemplate(remoteServer)
	template.Spec.DeclaredTools = nil
	fixture.catalog.Set(template, nil)

	fixture.upstream = fixture.startRemoteUpstream(t, accepted...)

	return fixture
}

func (f *remoteFixture) startRemoteUpstream(t *testing.T, accepted ...string) *remoteUpstream {
	t.Helper()

	upstream := &remoteUpstream{accepted: map[string]bool{}}
	for _, key := range accepted {
		upstream.accepted["Bearer "+key] = true
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "0"}, nil)
	mcpServer.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Text string `json:"text"`
			}

			if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
				return nil, err
			}

			upstream.mu.Lock()
			upstream.calls = append(upstream.calls, request.Extra.Header.Get("Authorization"))
			upstream.sessions = append(upstream.sessions, request.Session.ID())
			upstream.mu.Unlock()

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Text}}}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)

	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstream.mu.Lock()
		allowed := upstream.accepted[request.Header.Get("Authorization")]

		if allowed && request.Method == http.MethodDelete {
			upstream.closed++
		}
		upstream.mu.Unlock()

		if !allowed {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(writer, request)
	}))

	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})

	f.upstreams.Store(remoteServer, httpServer.URL)

	return upstream
}

// setSecret sets principal's remote server key, both as the stored secret
// the enable checks and as the value the headers are filled in from.
func (f *remoteFixture) setSecret(t *testing.T, principal, value string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(),
		`INSERT INTO user_secrets (principal, name, ciphertext, key_id, updated_at) VALUES ($1, $2, '\x00', 1, now())`,
		principal, remoteSecret)
	require.NoError(t, err)

	f.secrets.mu.Lock()
	defer f.secrets.mu.Unlock()

	if f.secrets.values[principal] == nil {
		f.secrets.values[principal] = map[string]string{}
	}

	f.secrets.values[principal][remoteSecret] = value
}

// stopAndNotify stops an instance as the idle stop does, and notifies.
func (f *remoteFixture) stopAndNotify(t *testing.T, id string) {
	t.Helper()

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "UPDATE instances SET status = 'stopped', desired = 'stopped' WHERE id = $1", id); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, id)
	}))
}

func TestRemoteServers(t *testing.T) {
	t.Run("with the secret set, a user lists and calls a tool and the call carries the user's key", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key")
		fixture.setSecret(t, "alice", "alice-key")

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, remoteServer, "user:alice", "echo")

		assert.Empty(t, instance.Namespace, "a remote server's instance has no namespace")
		assert.Contains(t, toolNames(t, client.session), "mail-archiver__echo")

		result := callTool(t, client.session, "mail-archiver__echo", map[string]any{"text": "hello"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hello", resultText(result))
		assert.Equal(t, []string{"Bearer alice-key"}, fixture.upstream.callAuthorizations())
	})

	t.Run("an enable with the secret missing gets the set-secret message and starts nothing", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key")

		client := fixture.connectObserved(t, alice())
		result := awaitEnable(t, startEnable(t, client.session, remoteServer))

		assert.Equal(t, "Secret `mail-archiver-api-key` isn't set. Run `toolgate secret set mail-archiver-api-key`.", result.err)
		assert.Empty(t, fixture.instancesOf(t, fixture.catalogTemplate(t, remoteServer)))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("a call with a refused credential says the credential was refused and how to set it", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key")
		fixture.setSecret(t, "alice", "wrong-key")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, remoteServer, "user:alice", "echo")

		result := callTool(t, client.session, "mail-archiver__echo", map[string]any{"text": "hello"})
		require.True(t, result.IsError)
		assert.Equal(t, "The remote server refused your credential (401 or 403). Check the secret and set it again with `toolgate secret set mail-archiver-api-key`.", resultText(result))
		assert.Empty(t, fixture.upstream.callAuthorizations())

		results := fixture.auditRows(t, "tool.result")
		require.Len(t, results, 1)
		assert.Equal(t, "failed", results[0].Details["outcome"])
	})

	t.Run("stopping the instance after a disable closes the forwarding session", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key")
		fixture.setSecret(t, "alice", "alice-key")

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, remoteServer, "user:alice", "echo")

		result := callTool(t, client.session, "mail-archiver__echo", map[string]any{"text": "hello"})
		require.False(t, result.IsError, resultText(result))

		_, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: gateway.ToolDisableServer, Arguments: map[string]any{"name": remoteServer},
		})
		require.NoError(t, err)
		assert.Zero(t, fixture.upstream.closedSessions(), "the instance is still running")

		fixture.stopAndNotify(t, instance.ID)

		require.Eventually(t, func() bool { return fixture.upstream.closedSessions() == 1 }, waitTimeout, 10*time.Millisecond)
	})

	t.Run("a second user's call uses the second user's key, never the first user's", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key", "bob-key")
		fixture.setSecret(t, "alice", "alice-key")
		fixture.setSecret(t, "bob", "bob-key")

		first := fixture.connectObserved(t, alice())
		aliceInstance := fixture.enableWithTools(t, first, remoteServer, "user:alice", "echo")
		require.False(t, callTool(t, first.session, "mail-archiver__echo", map[string]any{"text": "a"}).IsError)

		second := fixture.connectObserved(t, bob())
		bobInstance := fixture.enableWithTools(t, second, remoteServer, "user:bob", "echo")
		require.NotEqual(t, aliceInstance.ID, bobInstance.ID)

		result := callTool(t, second.session, "mail-archiver__echo", map[string]any{"text": "b"})
		require.False(t, result.IsError, resultText(result))
		require.False(t, callTool(t, first.session, "mail-archiver__echo", map[string]any{"text": "a"}).IsError)

		assert.Equal(t, []string{"Bearer alice-key", "Bearer bob-key", "Bearer alice-key"}, fixture.upstream.callAuthorizations())
	})

	t.Run("a remote server without per-user headers is shared: two users use one upstream session", func(t *testing.T) {
		fixture := newRemoteFixture(t, "static-key")

		template := fixture.catalogTemplate(t, remoteServer).DeepCopy()
		template.Spec.Scope = ""
		template.Spec.Remote.Headers = map[string]string{"Authorization": "Bearer static-key"}
		fixture.catalog.Set(template, nil)
		require.Equal(t, "shared", fixture.catalogTemplate(t, remoteServer).Spec.Scope)

		first := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, first, remoteServer, "shared", "echo")
		assert.Empty(t, instance.OwnerPrincipal)

		second := fixture.connectObserved(t, bob())
		require.Empty(t, awaitEnable(t, startEnable(t, second.session, remoteServer)).err)
		assert.Len(t, fixture.instancesOf(t, fixture.catalogTemplate(t, remoteServer)), 1)

		for _, client := range []*observedClient{first, second} {
			result := callTool(t, client.session, "mail-archiver__echo", map[string]any{"text": "hi"})
			require.False(t, result.IsError, resultText(result))
		}

		sessions := fixture.upstream.callSessions()
		require.Len(t, sessions, 2)
		assert.Equal(t, sessions[0], sessions[1], "both users' calls use one upstream session")
	})

	t.Run("a remote server's instance counts toward no instance limit", func(t *testing.T) {
		fixture := newRemoteFixtureWith(t, func(options *gateway.MCPOptions) { options.InstanceLimitPerUser = 1 }, "alice-key")
		fixture.setSecret(t, "alice", "alice-key")
		fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, remoteServer, "user:alice", "echo")
		fixture.enableWithTools(t, client, "github", "user:alice", "search")
	})

	t.Run("header values appear in no log line, audit line or audit row", func(t *testing.T) {
		fixture := newRemoteFixture(t, "alice-key")
		fixture.setSecret(t, "alice", "alice-key")
		fixture.setSecret(t, "bob", "bob-wrong-key")

		first := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, first, remoteServer, "user:alice", "echo")
		require.False(t, callTool(t, first.session, "mail-archiver__echo", map[string]any{"text": "a"}).IsError)

		second := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, second, remoteServer, "user:bob", "echo")
		require.True(t, callTool(t, second.session, "mail-archiver__echo", map[string]any{"text": "b"}).IsError)

		rows, err := fixture.pool.Query(t.Context(), "SELECT details::text FROM audit_events")
		require.NoError(t, err)

		details, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.NotEmpty(t, details)
		require.NotEmpty(t, fixture.audit.String())

		for _, value := range []string{"alice-key", "bob-wrong-key"} {
			assert.NotContains(t, fixture.logs.String(), value)
			assert.NotContains(t, fixture.audit.String(), value)

			for _, row := range details {
				assert.NotContains(t, row, value)
			}
		}
	})
}
