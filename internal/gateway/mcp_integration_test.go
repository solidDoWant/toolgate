//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const templateNamespace = "toolgate-servers"

type mcpFixture struct {
	provider *testutil.OIDCProvider
	pool     *pgxpool.Pool
	catalog  *catalog.Catalog
	server   *httptest.Server
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})

	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audience: provider.Audience})
	require.NoError(t, err)

	servers := catalog.New()
	handler := gateway.NewMCPHandler(gateway.MCPOptions{
		Database: pool,
		Catalog:  servers,
		Policy:   &policy.Engine{Rules: policy.NewRules()},
	})

	server := httptest.NewServer(authenticator.Middleware(handler, true))
	t.Cleanup(server.Close)

	return &mcpFixture{provider: provider, pool: pool, catalog: servers, server: server}
}

func (f *mcpFixture) addTemplate(name, scope string, access v1alpha1.Access) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: name, Generation: 2, UID: types.UID("uid-" + name)},
		Spec:       v1alpha1.MCPServerTemplateSpec{Description: "The " + name + " server.", Scope: scope, Access: access},
	}
	f.catalog.Set(template, nil)

	return template
}

// user is one signed-in client of the gateway.
type user struct {
	claims    testutil.TokenClaims
	session   string
	workspace string
}

func (f *mcpFixture) headers(t *testing.T, client user) http.Header {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+f.provider.MintAccessToken(t, client.claims))
	header.Set(auth.SessionHeader, client.session)

	if client.workspace != "" {
		header.Set(auth.WorkspaceHeader, client.workspace)
	}

	return header
}

type headerTransport struct {
	header http.Header
}

func (h headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range h.header {
		request.Header[name] = values
	}

	return http.DefaultTransport.RoundTrip(request)
}

func (f *mcpFixture) connect(t *testing.T, client user) *mcp.ClientSession {
	t.Helper()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   f.server.URL,
		HTTPClient: &http.Client{Transport: headerTransport{header: f.headers(t, client)}},
	}

	session, err := mcpClient.Connect(t.Context(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// post sends one raw MCP message and returns the response.
func (f *mcpFixture) post(t *testing.T, client user, mcpSessionID string, message map[string]any) *http.Response {
	t.Helper()

	body, err := json.Marshal(message)
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL, bytes.NewReader(body))
	require.NoError(t, err)

	request.Header = f.headers(t, client)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	if mcpSessionID != "" {
		request.Header.Set("Mcp-Session-Id", mcpSessionID)
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

func initializeMessage() map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "raw", "version": "0"},
		},
	}
}

func listServers(t *testing.T, session *mcp.ClientSession) []gateway.ServerEntry {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolListServers})
	require.NoError(t, err)
	require.False(t, result.IsError, "list_servers failed: %v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var decoded gateway.ListServersResult
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	return decoded.Servers
}

func (f *mcpFixture) sessionRow(t *testing.T, principal, session string) db.Session {
	t.Helper()

	row, err := db.New(f.pool).GetSession(t.Context(), db.GetSessionParams{Principal: principal, SessionID: session})
	require.NoError(t, err)

	return row
}

var instanceCounter int

// addInstance inserts an instance row for template and scope key and moves
// it to status ("ready" or "failed").
func (f *mcpFixture) addInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, scopeKey string, generation int64, status, reason string) {
	t.Helper()

	instanceCounter++
	queries := db.New(f.pool)
	id := fmt.Sprintf("%032x", instanceCounter)

	_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		Generation: generation, Scope: template.Spec.Scope, ScopeKey: scopeKey, Namespace: "instance-" + id[24:],
	})
	require.NoError(t, err)

	switch status {
	case "ready":
		_, err = queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: id, Generation: generation})
		require.NoError(t, err)
		_, err = queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: id, Tools: []byte(`[]`)})
		require.NoError(t, err)
	case "failed":
		_, err = queries.SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: id, Reason: reason, FromStatus: "pending"})
		require.NoError(t, err)
	}
}

func alice() user {
	return user{
		claims:  testutil.TokenClaims{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}},
		session: "session-1",
	}
}

func bob() user {
	return user{claims: testutil.TokenClaims{Subject: "bob"}, session: "session-1"}
}

func TestMCPEndpointOffersMetaTools(t *testing.T) {
	fixture := newMCPFixture(t)
	session := fixture.connect(t, alice())

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	var names []string
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}

	assert.ElementsMatch(t, []string{
		gateway.ToolListServers, gateway.ToolEnableServer, gateway.ToolDisableServer, gateway.ToolServerStatus,
	}, names)
}

func TestMCPEndpointRefusesUnauthenticated(t *testing.T) {
	fixture := newMCPFixture(t)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.server.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set(auth.SessionHeader, "session-1")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

func TestListServers(t *testing.T) {
	t.Run("[AC-1-06] returns exactly the templates the identity may use, with scope, enabled state and status", func(t *testing.T) {
		fixture := newMCPFixture(t)

		bySubject := fixture.addTemplate("by-subject", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
		byGroup := fixture.addTemplate("by-group", "shared", v1alpha1.Access{Groups: []string{"eng"}, Environments: "any"})
		fixture.addTemplate("by-email", "per-session", v1alpha1.Access{Users: []string{"alice@example.com"}, Environments: "any"})
		fixture.addTemplate("attested-only", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "attested"})
		fixture.addTemplate("other-user", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
		fixture.addTemplate("other-group", "shared", v1alpha1.Access{Groups: []string{"ops"}, Environments: "any"})
		fixture.addTemplate("service-account-only", "shared", v1alpha1.Access{
			ServiceAccounts: []string{"system:serviceaccount:ci:alice"}, Environments: "any",
		})

		session := fixture.connect(t, alice())
		row := fixture.sessionRow(t, "alice", "session-1")

		fixture.addInstance(t, bySubject, "user:alice", 2, "ready", "")
		fixture.addInstance(t, byGroup, "shared", 2, "failed", "Image `example/server:1` can't be pulled.")
		require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "by-subject",
		}))

		assert.ElementsMatch(t, []gateway.ServerEntry{
			{Name: "by-subject", Description: "The by-subject server.", Scope: "per-user", Enabled: true, Status: "ready"},
			{Name: "by-group", Description: "The by-group server.", Scope: "shared", Status: "failed", Reason: "Image `example/server:1` can't be pulled."},
			{Name: "by-email", Description: "The by-email server.", Scope: "per-session", Status: "stopped"},
			{
				Name: "attested-only", Description: "The attested-only server.", Scope: "per-user", Status: "blocked",
				Reason: "Server \"attested-only\" can be used only from an attested environment, not from a signed-in user's machine.",
			},
		}, listServers(t, session))
	})

	t.Run("an unverified email gives no access", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addTemplate("by-email", "per-session", v1alpha1.Access{Users: []string{"carol@example.com"}, Environments: "any"})

		session := fixture.connect(t, user{
			claims:  testutil.TokenClaims{Subject: "carol", Email: "carol@example.com"},
			session: "session-1",
		})

		assert.Empty(t, listServers(t, session))
	})
}

func TestServerStatus(t *testing.T) {
	fixture := newMCPFixture(t)
	template := fixture.addTemplate("by-subject", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
	fixture.addTemplate("other-user", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
	fixture.addInstance(t, template, "user:alice", 1, "ready", "")

	session := fixture.connect(t, alice())

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: gateway.ToolServerStatus, Arguments: map[string]any{"name": "by-subject"},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "server_status failed: %v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var status gateway.ServerStatusResult
	require.NoError(t, json.Unmarshal(encoded, &status))

	assert.Equal(t, "ready", status.Status)
	assert.True(t, status.Outdated, "an instance of generation 1 of a generation 2 template is outdated")
	require.NotNil(t, status.ReadyAt)
	assert.WithinDuration(t, time.Now(), *status.ReadyAt, time.Minute)

	for _, name := range []string{"other-user", "missing"} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: gateway.ToolServerStatus, Arguments: map[string]any{"name": name},
		})
		require.NoError(t, err)
		assert.True(t, result.IsError, "server_status of %q must fail", name)
	}
}

func TestSessions(t *testing.T) {
	t.Run("a different workspace on a live session gets 400", func(t *testing.T) {
		fixture := newMCPFixture(t)

		first := alice()
		first.workspace = "repo-a"
		fixture.connect(t, first)

		second := alice()
		second.workspace = "repo-b"
		response := fixture.post(t, second, "", initializeMessage())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)

		assert.Equal(t, "claimed/repo-a", fixture.sessionRow(t, "alice", "session-1").Workspace)
	})

	t.Run("one session ID used by two principals gives separate sessions", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addTemplate("shared-tool", "shared", v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"})

		aliceSession := fixture.connect(t, alice())
		bobSession := fixture.connect(t, bob())

		aliceRow := fixture.sessionRow(t, "alice", "session-1")
		bobRow := fixture.sessionRow(t, "bob", "session-1")
		require.NotEqual(t, aliceRow.ID, bobRow.ID)

		require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: aliceRow.ID, TemplateNs: templateNamespace, TemplateName: "shared-tool",
		}))

		aliceServers := listServers(t, aliceSession)
		require.Len(t, aliceServers, 1)
		assert.True(t, aliceServers[0].Enabled)

		bobServers := listServers(t, bobSession)
		require.Len(t, bobServers, 1)
		assert.False(t, bobServers[0].Enabled)
	})

	t.Run("a request for another principal on an MCP session gets 404", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.post(t, alice(), "", initializeMessage())
		require.Equal(t, http.StatusOK, response.StatusCode)

		mcpSessionID := response.Header.Get("Mcp-Session-Id")
		require.NotEmpty(t, mcpSessionID)

		listTools := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}

		assert.Equal(t, http.StatusNotFound, fixture.post(t, bob(), mcpSessionID, listTools).StatusCode)

		otherSession := alice()
		otherSession.session = "session-2"
		assert.Equal(t, http.StatusNotFound, fixture.post(t, otherSession, mcpSessionID, listTools).StatusCode)

		assert.Equal(t, http.StatusNotFound, fixture.post(t, alice(), "unknown-session", listTools).StatusCode)
	})

	t.Run("a request for an ended session restarts it with the new workspace", func(t *testing.T) {
		fixture := newMCPFixture(t)

		first := alice()
		first.workspace = "repo-a"
		fixture.connect(t, first)

		row := fixture.sessionRow(t, "alice", "session-1")
		_, err := fixture.pool.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)

		second := alice()
		second.workspace = "repo-b"
		fixture.connect(t, second)

		restarted := fixture.sessionRow(t, "alice", "session-1")
		assert.Equal(t, row.ID, restarted.ID)
		assert.False(t, restarted.EndedAt.Valid)
		assert.Equal(t, "claimed/repo-b", restarted.Workspace)

		var starts int
		require.NoError(t, fixture.pool.QueryRow(context.WithoutCancel(t.Context()),
			"SELECT count(*) FROM audit_events WHERE type = 'session.start' AND principal = 'alice'").Scan(&starts))
		assert.Equal(t, 2, starts)
	})
}
