//go:build integration

package gateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

const (
	aliceWorkspace = "coder/alice/dev"
	approvalPrefix = "https://toolgate.example.com/ui/approvals/"
)

func attestedUsers() *auth.Options {
	return &auth.Options{Attestation: &auth.AttestationOptions{
		Client:      newAttestationClient(),
		Namespaces:  []string{"coder"},
		MatchBy:     auth.MatchByEmail,
		WorkspaceID: "coder/{username}/{workspace}",
	}}
}

// attestedAlice is alice in her verified workspace coder/alice/dev.
func attestedAlice() user {
	client := alice()
	client.attestation = workloadToken

	return client
}

// aliceIdentity is alice as the approval page sees her.
func aliceIdentity() auth.Identity {
	return auth.Identity{
		Principal: "alice", Email: "alice@example.com", Groups: []string{"eng"},
		Workspace: aliceWorkspace, Kind: auth.KindUser, Attestation: auth.AttestationAttested,
	}
}

// workspaceAPI is the workspace API over a fixture's database and catalog.
type workspaceAPI struct {
	fixture    *mcpFixture
	server     *httptest.Server
	workspaces *gateway.Workspaces
}

func newWorkspaceAPI(t *testing.T, fixture *mcpFixture, enabled bool) *workspaceAPI {
	t.Helper()

	options := attestedUsers()
	options.IssuerURL, options.Audiences = fixture.provider.Issuer, []string{fixture.provider.Audience}

	authenticator, err := auth.New(t.Context(), *options)
	require.NoError(t, err)

	workspaces := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0)
	mux := http.NewServeMux()
	(&workspacesapi.Handler{
		Workspaces: workspaces, Enabled: enabled,
		ApprovalURL: func(id string) string { return approvalPrefix + id },
	}).Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &workspaceAPI{fixture: fixture, server: server, workspaces: workspaces}
}

// do sends one request as client, or without credentials when client is
// nil, and returns the status and body.
func (a *workspaceAPI) do(t *testing.T, client *user, method, path string, body any) (int, string) {
	t.Helper()

	var reader io.Reader

	switch typed := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(typed)
	default:
		encoded, err := json.Marshal(body)
		require.NoError(t, err)

		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(t.Context(), method, a.server.URL+path, reader)
	require.NoError(t, err)

	if client != nil {
		request.Header = a.fixture.headers(t, *client)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	read, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, strings.TrimSpace(string(read))
}

func (a *workspaceAPI) change(t *testing.T, client user, change workspacesapi.Change) (int, string) {
	t.Helper()

	return a.do(t, &client, http.MethodPost, workspacesapi.ChangesPath, change)
}

// requestChange sends a widening change and returns its pending request.
func (a *workspaceAPI) requestChange(t *testing.T, client user, change workspacesapi.Change) string {
	t.Helper()

	status, body := a.change(t, client, change)
	require.Equal(t, http.StatusAccepted, status, body)

	var requested workspacesapi.Requested
	require.NoError(t, json.Unmarshal([]byte(body), &requested))
	require.Regexp(t, `^[0-9a-f]{32}$`, requested.Request)
	assert.Equal(t, approvalPrefix+requested.Request, requested.URL)
	assert.Equal(t, "pending", a.requestStatus(t, client, requested.Request))

	return requested.Request
}

// applyChange sends a narrowing change, which must apply at once.
func (a *workspaceAPI) applyChange(t *testing.T, client user, change workspacesapi.Change) {
	t.Helper()

	status, body := a.change(t, client, change)
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `{"applied": true}`, body)
}

func (a *workspaceAPI) requestStatus(t *testing.T, client user, id string) string {
	t.Helper()

	status, body := a.do(t, &client, http.MethodGet, workspacesapi.AccessRequestsPath+id, nil)
	require.Equal(t, http.StatusOK, status, body)

	var request workspacesapi.AccessRequest
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	assert.Equal(t, id, request.ID)
	assert.True(t, request.ExpiresAt.After(time.Now().Add(-time.Minute)))

	return request.Status
}

// approve approves alice's change request through the function the
// approval page calls.
func (a *workspaceAPI) approve(t *testing.T, id string) error {
	t.Helper()

	return pgx.BeginFunc(t.Context(), a.fixture.pool, func(tx pgx.Tx) error {
		return a.workspaces.ApproveChangeRequest(t.Context(), tx, aliceIdentity(), id)
	})
}

// widen requests a widening change and approves it.
func (a *workspaceAPI) widen(t *testing.T, client user, change workspacesapi.Change) {
	t.Helper()

	id := a.requestChange(t, client, change)
	require.NoError(t, a.approve(t, id))
	assert.Equal(t, "approved", a.requestStatus(t, client, id))
}

func (a *workspaceAPI) list(t *testing.T, client user) workspacesapi.List {
	t.Helper()

	status, body := a.do(t, &client, http.MethodGet, workspacesapi.WorkspacesPath, nil)
	require.Equal(t, http.StatusOK, status, body)

	var list workspacesapi.List
	require.NoError(t, json.Unmarshal([]byte(body), &list))

	return list
}

// lists maps each of the user's workspace lists to its servers, leaving out
// workspaces that have scope entries but no list.
func (a *workspaceAPI) lists(t *testing.T, client user) map[string][]string {
	t.Helper()

	lists := map[string][]string{}

	for _, each := range a.list(t, client).Workspaces {
		// A workspace with only scope entries has no list.
		if each.Servers != nil {
			lists[each.Workspace] = each.Servers
		}
	}

	return lists
}

func (f *mcpFixture) sessionAdditions(t *testing.T, sessionPK int64) []string {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), `SELECT server FROM session_additions WHERE session_pk = $1 ORDER BY server`, sessionPK)
	require.NoError(t, err)

	servers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)

	return servers
}

func (f *mcpFixture) addSessionAddition(t *testing.T, sessionPK int64, server string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `INSERT INTO session_additions (session_pk, server, request_id) VALUES ($1, $2, 'test')`, sessionPK, server)
	require.NoError(t, err)
}

func entryNamed(entries []gateway.ServerEntry, name string) (gateway.ServerEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}

	return gateway.ServerEntry{}, false
}

func verifiedRefusal(server string) string {
	return fmt.Sprintf("Server `%s` isn't in the list for workspace `%s`. Allow it in the Workspaces page or with `toolgate workspace allow %s`.",
		server, aliceWorkspace, server)
}

func assertBlockedInWorkspace(t *testing.T, entries []gateway.ServerEntry, name string) {
	t.Helper()

	entry, ok := entryNamed(entries, name)
	require.True(t, ok, "%s is listed", name)
	assert.Equal(t, "blocked", entry.Status)
	assert.Equal(t, "not allowed in this workspace; enable it to ask", entry.Reason)
}

func assertNotBlocked(t *testing.T, entries []gateway.ServerEntry, name string) {
	t.Helper()

	entry, ok := entryNamed(entries, name)
	require.True(t, ok, "%s is listed", name)
	assert.NotEqual(t, "blocked", entry.Status)
}

func newScopedFixture(t *testing.T) (*mcpFixture, *workspaceAPI) {
	t.Helper()

	fixture := newMCPFixtureWithAuth(t, attestedUsers(), withWorkspaceScoping)

	return fixture, newWorkspaceAPI(t, fixture, true)
}

var aliceAndBob = v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"}

func TestWorkspaceLists(t *testing.T) {
	t.Run("a CLI allow applies only after approval, the list filters servers, tools and calls, and a deny withdraws within 5 s across replicas", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, attestedAlice())
		assertBlockedInWorkspace(t, listServers(t, client.session), "github")

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})

		list := api.list(t, attestedAlice())
		require.NotNil(t, list.Current)
		assert.Equal(t, aliceWorkspace, *list.Current)
		assert.Equal(t, workspacesapi.Defaults{Verified: "none", Unverified: "none", UnverifiedServers: []string{}}, list.Defaults)
		assert.Empty(t, list.Workspaces, "a widening change doesn't apply before approval")
		assertBlockedInWorkspace(t, listServers(t, client.session), "github")

		require.NoError(t, api.approve(t, id))
		assert.Equal(t, "approved", api.requestStatus(t, attestedAlice(), id))
		require.ErrorIs(t, api.approve(t, id), gateway.ErrAccessRequestUndecidable, "an approved request can't be approved again")
		assert.Equal(t, map[string][]string{aliceWorkspace: {"github"}}, api.lists(t, attestedAlice()))

		entries := listServers(t, client.session)
		assertNotBlocked(t, entries, "github")
		assertBlockedInWorkspace(t, entries, "slack")

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		allowed := callTool(t, client.session, "github__echo", map[string]any{"text": "in"})
		require.False(t, allowed.IsError, resultText(allowed))

		other := fixture.replica(t)
		elsewhere := fixture.connectObservedTo(t, other.URL, attestedAlice())
		require.Contains(t, toolNames(t, elsewhere.session), "github__echo")

		before, beforeElsewhere := client.listChangedCount(), elsewhere.listChangedCount()
		started := time.Now()

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, scopeChangeBound, 10*time.Millisecond)
		require.Eventually(t, func() bool { return elsewhere.listChangedCount() > beforeElsewhere }, scopeChangeBound, 10*time.Millisecond,
			"the session on the other replica got no list_changed within 5 s")
		assert.Less(t, time.Since(started), scopeChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, elsewhere.session))

		// A deny also inserts a deny entry (docs/design/phase-4.md §7), so the
		// refusal names the deny.
		refused := callTool(t, elsewhere.session, "github__echo", map[string]any{"text": "out"})
		require.True(t, refused.IsError)
		assert.Equal(t, "`github__echo` is denied in workspace `"+aliceWorkspace+"`.", resultText(refused))

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		require.Len(t, changes, 2)
		assert.Equal(t, id, changes[0].Details["via"])
		assert.Equal(t, []any{"github"}, changes[0].Details["added"])
		assert.Equal(t, gateway.ViaCLI, changes[1].Details["via"])
		assert.Equal(t, []any{"github"}, changes[1].Details["removed"])
		assert.Equal(t, aliceWorkspace, changes[1].Workspace)

		requests := fixture.auditRows(t, gateway.TypeAccessRequest)
		require.Len(t, requests, 1)
		assert.Equal(t, id, requests[0].Details["request"])
		approvals := fixture.auditRows(t, gateway.TypeAccessApprove)
		require.Len(t, approvals, 1)
		assert.Equal(t, id, approvals[0].Details["request"])
	})

	t.Run("allow of a server outside catalog access is refused and inserts nothing", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})

		for _, server := range []string{"github", "missing"} {
			status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{server}})
			assert.Equal(t, http.StatusForbidden, status)
			assert.Equal(t, "You don't have access to server `"+server+"`.", body)
		}

		assert.Zero(t, fixture.accessRequestCount(t))
		assert.Empty(t, api.lists(t, attestedAlice()))
	})

	t.Run("row 125: a listed server the user loses access to stays refused", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})

		template.Spec.Access = v1alpha1.Access{Users: []string{"bob"}, Environments: "any"}
		fixture.catalog.Set(template, nil)

		client := fixture.connectObserved(t, attestedAlice())
		_, listed := entryNamed(listServers(t, client.session), "github")
		assert.False(t, listed)

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assert.NotContains(t, resultText(result), "workspace")
		assert.Equal(t, map[string][]string{aliceWorkspace: {"github"}}, api.lists(t, attestedAlice()), "the entry stays")
	})

	t.Run("deny with no list makes no list, denying the last server keeps an empty list, and only clear deletes it", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		// Phase 4 drops phase 3's 409: the deny inserts a deny entry
		// (docs/design/phase-4.md §7, row 206).
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})
		assert.Empty(t, api.lists(t, attestedAlice()))

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"slack"}})
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"slack"}})
		assert.Equal(t, map[string][]string{aliceWorkspace: {}}, api.lists(t, attestedAlice()))

		// A deny entry outlasts the list; delete slack's, so the list and
		// the default alone decide slack below.
		_, err := fixture.pool.Exec(t.Context(), `DELETE FROM scope_entries WHERE server = 'slack'`)
		require.NoError(t, err)

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "all"})

		client := fixture.connectObserved(t, attestedAlice())
		assertBlockedInWorkspace(t, listServers(t, client.session), "slack")

		// With the default all, clearing the list widens; the deny entry stays
		// (row 243).
		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "clear"})
		assert.Equal(t, map[string][]string{aliceWorkspace: {}}, api.lists(t, attestedAlice()))
		require.NoError(t, api.approve(t, id))
		assert.Empty(t, api.lists(t, attestedAlice()))
		assertNotBlocked(t, listServers(t, client.session), "slack")
		assertDenied(t, listServers(t, client.session), "github", "denied in this workspace")

		cleared := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		assert.Equal(t, true, cleared[len(cleared)-1].Details["cleared"])

		// With the default none, clearing narrows.
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "none"})
		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "clear", Workspace: aliceWorkspace})
		assert.Empty(t, api.lists(t, attestedAlice()))
	})

	t.Run("row 128: deny deletes the server's session additions of the user's open sessions in that workspace", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")
		fixture.addSessionAddition(t, session.ID, "github")
		fixture.addSessionAddition(t, session.ID, "slack")

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"slack"}})
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})

		assert.Equal(t, []string{"slack"}, fixture.sessionAdditions(t, session.ID))
	})

	t.Run("rows 161, 164: deny --session removes that session's additions only, keeps the lists, and gets 404 for another session", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")
		fixture.addSessionAddition(t, session.ID, "github")
		fixture.addSessionAddition(t, session.ID, "slack")
		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: "session-1", Servers: []string{"github"}})

		assert.Equal(t, []string{"slack"}, fixture.sessionAdditions(t, session.ID))
		assert.Equal(t, map[string][]string{aliceWorkspace: {"github"}}, api.lists(t, attestedAlice()), "the lists stay as they are")

		removed := fixture.auditRows(t, gateway.TypeSessionAddition)
		require.Len(t, removed, 1)
		assert.Equal(t, "github", removed[0].Server)
		assert.Equal(t, "session-1", removed[0].Session)
		assert.Equal(t, true, removed[0].Details["removed"])

		bobs := bob()
		bobs.session = "bob-session"
		fixture.connectObserved(t, bobs)

		for _, other := range []string{"bob-session", "no-such-session"} {
			status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: other, Servers: []string{"slack"}})
			assert.Equal(t, http.StatusNotFound, status, body)
		}

		status, _ := api.change(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: "session-1", Workspace: aliceWorkspace, Servers: []string{"slack"}})
		assert.Equal(t, http.StatusBadRequest, status, "session and workspace can't be combined")
		assert.Equal(t, []string{"slack"}, fixture.sessionAdditions(t, session.ID))
	})

	t.Run("row 127: concurrent writes to the same principal's lists run in turn as set operations", func(t *testing.T) {
		fixture, api := newScopedFixture(t)

		const writers = 16

		servers := make([]string, 0, writers)
		for index := range writers {
			name := fmt.Sprintf("server-%02d", index)
			fixture.addTemplate(name, "per-user", aliceAndBob)
			servers = append(servers, name)
		}

		// The row exists first, so only its lock orders the writers.
		require.NoError(t, store.EnsureUserScope(t.Context(), fixture.pool, "alice"))

		var group sync.WaitGroup

		errs := make(chan error, writers)

		for _, server := range servers {
			group.Go(func() {
				errs <- api.workspaces.Apply(t.Context(), aliceIdentity(), gateway.WorkspaceChange{
					Action: gateway.WorkspaceAllow, Workspace: aliceWorkspace, Servers: []string{server},
				}, gateway.ViaUI)
			})
		}

		group.Wait()
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}

		assert.Equal(t, map[string][]string{aliceWorkspace: servers}, api.lists(t, attestedAlice()))
	})

	t.Run("the list and server limits hold at exactly their size", func(t *testing.T) {
		fixture, api := newScopedFixture(t)

		servers := make([]string, 0, gateway.MaxWorkspaceListServers+1)
		for index := range gateway.MaxWorkspaceListServers + 1 {
			name := fmt.Sprintf("server-%02d", index)
			fixture.addTemplate(name, "per-user", aliceAndBob)
			servers = append(servers, name)
		}

		identity := aliceIdentity()
		apply := func(change gateway.WorkspaceChange) error {
			return api.workspaces.Apply(t.Context(), identity, change, gateway.ViaUI)
		}

		require.NoError(t, apply(gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "full", Servers: servers[:gateway.MaxWorkspaceListServers]}))

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, apply(gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "full", Servers: servers[gateway.MaxWorkspaceListServers:]}), &refusal)
		assert.Equal(t, "Workspace `full`'s list already has 64 servers. Remove one, then try again.", refusal.Message)

		for index := 1; index < gateway.MaxWorkspaceLists; index++ {
			require.NoError(t, apply(gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: fmt.Sprintf("w-%03d", index), Servers: servers[:1]}))
		}

		assert.Len(t, api.lists(t, attestedAlice()), gateway.MaxWorkspaceLists)
		require.ErrorAs(t, apply(gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "one-more", Servers: servers[:1]}), &refusal)
		assert.Equal(t, "You already have 200 workspace lists. Remove one, then try again.", refusal.Message)
		require.NoError(t, apply(gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "w-001", Servers: servers[1:2]}),
			"an existing list can still grow")
	})

	t.Run("a principal has at most 100 pending access requests", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addTemplate("github", "per-user", aliceAndBob)

		for index := range gateway.MaxPendingAccessRequestsPerPrincipal {
			result, err := api.workspaces.Request(t.Context(), aliceIdentity(), gateway.WorkspaceChange{
				Action: gateway.WorkspaceAllow, Workspace: fmt.Sprintf("w-%03d", index), Servers: []string{"github"},
			})
			require.NoError(t, err)
			require.NotEmpty(t, result.Request)
		}

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "You already have 100 pending access requests. Approve or deny some first.", body)
		assert.Equal(t, gateway.MaxPendingAccessRequestsPerPrincipal, fixture.accessRequestCount(t))
	})

	t.Run("the API refuses unauthenticated callers, other kinds, other users' requests and bad bodies, and is off without the web UI", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addTemplate("github", "per-user", aliceAndBob)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})

		for _, route := range []struct{ method, path string }{
			{http.MethodGet, workspacesapi.WorkspacesPath},
			{http.MethodPost, workspacesapi.ChangesPath},
			{http.MethodGet, workspacesapi.AccessRequestsPath + id},
		} {
			status, _ := api.do(t, nil, route.method, route.path, `{"action":"clear"}`)
			assert.Equal(t, http.StatusUnauthorized, status, "%s %s", route.method, route.path)
		}

		bobs := bob()
		status, _ := api.do(t, &bobs, http.MethodGet, workspacesapi.AccessRequestsPath+id, nil)
		assert.Equal(t, http.StatusNotFound, status, "only the requester reads a request")

		_, err := api.workspaces.Request(t.Context(), auth.Identity{
			Principal: "system:serviceaccount:ci:runner", Kind: auth.KindServiceAccount,
			Workspace: "k8s/ci/Pod/runner", Attestation: auth.AttestationAttested,
		}, gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Verified: "none"})

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusForbidden, refusal.Status)

		status, _ = api.do(t, ptr(attestedAlice()), http.MethodPost, workspacesapi.ChangesPath, `{"action":`)
		assert.Equal(t, http.StatusBadRequest, status)
		status, _ = api.do(t, ptr(attestedAlice()), http.MethodPost, workspacesapi.ChangesPath,
			`{"action":"allow","servers":["`+strings.Repeat("a", workspacesapi.MaxChangeBytes)+`"]}`)
		assert.Equal(t, http.StatusRequestEntityTooLarge, status)

		off := newWorkspaceAPI(t, fixture, false)
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, workspacesapi.WorkspacesPath},
			{http.MethodPost, workspacesapi.ChangesPath},
		} {
			status, body := off.do(t, ptr(attestedAlice()), route.method, route.path, `{"action":"clear"}`)
			assert.Equal(t, http.StatusConflict, status)
			assert.Equal(t, workspacesapi.MessageScopingOff, body)
		}
	})
}

func ptr[T any](value T) *T { return &value }

func TestWorkspaceDefaults(t *testing.T) {
	t.Run("the verified default widens only after approval and narrows at once", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, attestedAlice())
		assertBlockedInWorkspace(t, listServers(t, client.session), "github")

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "all"})
		assert.Equal(t, workspacesapi.Defaults{Verified: "none", Unverified: "none", UnverifiedServers: []string{}}, api.list(t, attestedAlice()).Defaults)
		require.NoError(t, api.approve(t, id))
		assert.Equal(t, workspacesapi.Defaults{Verified: "all", Unverified: "none", UnverifiedServers: []string{}}, api.list(t, attestedAlice()).Defaults)

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		unverified := alice()
		unverified.session = "unverified"
		assert.Empty(t, listServers(t, fixture.connectObserved(t, unverified).session), "the unverified default stays none")

		before := client.listChangedCount()

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "none"})

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, scopeChangeBound, 10*time.Millisecond)
		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, refused.IsError)
		assert.Equal(t, verifiedRefusal("github"), resultText(refused))

		defaults := fixture.auditRows(t, gateway.TypeWorkspaceDefaults)
		require.Len(t, defaults, 2)
		assert.Equal(t, map[string]any{"verified": "none", "unverified": "none"}, defaults[0].Details["old"])
		assert.Equal(t, map[string]any{"verified": "all", "unverified": "none"}, defaults[0].Details["new"])
		assert.Equal(t, id, defaults[0].Details["via"])
		assert.Equal(t, map[string]any{"verified": "none", "unverified": "none"}, defaults[1].Details["new"])
	})

	t.Run("the unverified default widens only after approval and narrows at once", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		assert.Empty(t, listServers(t, client.session))

		id := api.requestChange(t, alice(), workspacesapi.Change{Action: "defaults", Unverified: workspacesapi.Unverified{Default: "all"}})
		assert.Empty(t, listServers(t, client.session))
		require.NoError(t, api.approve(t, id))

		assert.Equal(t, []string{"github"}, serverNames(listServers(t, client.session)))
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		before := client.listChangedCount()

		api.applyChange(t, alice(), workspacesapi.Change{Action: "defaults", Unverified: workspacesapi.Unverified{Default: "none"}})

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, scopeChangeBound, 10*time.Millisecond)
		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, refused.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(refused))
		assert.Equal(t, policy.ScopeDefaultNone, api.list(t, alice()).Defaults.Unverified)
	})
}
