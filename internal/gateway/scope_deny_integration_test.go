//go:build integration

package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// scopeEntryRows returns every scope entry as `<target> <server> <tool>
// <effect>`, where the target is the workspace or `session:<session ID>`.
func (f *mcpFixture) scopeEntryRows(t *testing.T) []string {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), `SELECT coalesce(e.workspace, 'session:' || s.session_id), e.server, e.tool, e.effect
		FROM scope_entries e LEFT JOIN sessions s ON s.id = e.session_pk ORDER BY e.id`)
	require.NoError(t, err)

	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var target, server, tool, effect string

		err := row.Scan(&target, &server, &tool, &effect)

		return fmt.Sprintf("%s %s %s %s", target, server, tool, effect), err
	})
	require.NoError(t, err)

	return entries
}

// workspaceNamed returns the workspace of the list body.
func workspaceNamed(t *testing.T, list workspacesapi.List, name string) workspacesapi.Workspace {
	t.Helper()

	for _, each := range list.Workspaces {
		if each.Workspace == name {
			return each
		}
	}

	require.Failf(t, "workspace not listed", "%s is not in %+v", name, list.Workspaces)

	return workspacesapi.Workspace{}
}

// curatedGithub adds github, accessible to alice and bob, whose upstream
// tool create_issue is exposed as open_issue.
func curatedGithub(fixture *mcpFixture) {
	template := fixture.addEnableTemplate("github", "per-user", aliceAndBob)
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}
	fixture.catalog.Set(template, nil)
}

// sessionsList reads `GET /api/v1/sessions` as alice, with the entries
// read by the API's workspaces service.
func (a *workspaceAPI) sessionsList(t *testing.T, client user) sessionsapi.List {
	t.Helper()

	options := attestedUsers()
	options.IssuerURL, options.Audiences = a.fixture.provider.Issuer, []string{a.fixture.provider.Audience}

	authenticator, err := auth.New(t.Context(), *options)
	require.NoError(t, err)

	mux := http.NewServeMux()
	(&sessionsapi.Handler{Queries: db.New(a.fixture.pool), IdleTimeout: time.Hour, Entries: a.workspaces}).
		Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+sessionsapi.Path, nil)
	require.NoError(t, err)

	request.Header = a.fixture.headers(t, client)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)

	var list sessionsapi.List
	require.NoError(t, json.NewDecoder(response.Body).Decode(&list))

	return list
}

func TestScopeDenyThroughTheAPI(t *testing.T) {
	t.Run("workspace, server: removes it from the list, inserts a deny entry and deletes its allow entries", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github", "slack"}})
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectAllow)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})

		assert.Equal(t, []string{aliceWorkspace + " github  deny"}, fixture.scopeEntryRows(t))

		listed := workspaceNamed(t, api.list(t, attestedAlice()), aliceWorkspace)
		assert.Equal(t, []string{"slack"}, listed.Servers)
		assert.Equal(t, []string{"github"}, listed.Denied)
		assert.Empty(t, listed.Tools)

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		assert.Equal(t, []any{"github"}, changes[len(changes)-1].Details["denied"])
		assert.Equal(t, []any{"github"}, changes[len(changes)-1].Details["removed"])
	})

	t.Run("workspace, tool: maps the exposed name, inserts a deny entry and deletes its allow entry", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectAllow)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "list_issues", policy.EffectAllow)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github__open_issue"}})

		assert.Equal(t, []string{
			aliceWorkspace + " github list_issues allow",
			aliceWorkspace + " github create_issue deny",
		}, fixture.scopeEntryRows(t))

		listed := workspaceNamed(t, api.list(t, attestedAlice()), aliceWorkspace)
		assert.Nil(t, listed.Servers, "the workspace has no list")
		assert.Equal(t, []string{"github__list_issues"}, listed.Tools)
		assert.Equal(t, []string{"github__open_issue"}, listed.Denied, "entries are shown by exposed name")
	})

	t.Run("session, server: deletes its session addition and allow entries and inserts a session deny entry", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")
		fixture.addSessionAddition(t, session.ID, "github")
		fixture.addSessionEntry(t, "session-1", "github", "create_issue", policy.EffectAllow)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: "session-1", Servers: []string{"github"}})

		assert.Empty(t, fixture.sessionAdditions(t, session.ID))
		assert.Equal(t, []string{"session:session-1 github  deny"}, fixture.scopeEntryRows(t))

		events := fixture.auditRows(t, gateway.TypeSessionAddition)
		require.Len(t, events, 1)
		assert.Equal(t, "github", events[0].Server)
		assert.Equal(t, true, events[0].Details["denied"])
		assert.Equal(t, true, events[0].Details["removed"])

		sessions := api.sessionsList(t, attestedAlice())
		require.Len(t, sessions.Sessions, 1)
		assert.Equal(t, []string{"github"}, sessions.Sessions[0].Denied)
		assert.Empty(t, sessions.Sessions[0].Allowed)
	})

	t.Run("session, tool: inserts a session deny entry for the upstream tool", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		fixture.connectObserved(t, attestedAlice())
		fixture.addSessionEntry(t, "session-1", "github", "list_issues", policy.EffectAllow)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: "session-1", Servers: []string{"github__open_issue"}})

		assert.Equal(t, []string{
			"session:session-1 github list_issues allow",
			"session:session-1 github create_issue deny",
		}, fixture.scopeEntryRows(t))

		sessions := api.sessionsList(t, attestedAlice())
		require.Len(t, sessions.Sessions, 1)
		assert.Equal(t, []string{"github__list_issues"}, sessions.Sessions[0].Allowed)
		assert.Equal(t, []string{"github__open_issue"}, sessions.Sessions[0].Denied)
	})

	t.Run("a deny may target the caller's unverified session", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		fixture.connectObserved(t, alice())

		api.applyChange(t, alice(), workspacesapi.Change{Action: "deny", Session: "session-1", Servers: []string{"github"}})

		assert.Equal(t, []string{"session:session-1 github  deny"}, fixture.scopeEntryRows(t))
	})

	t.Run("another user's session gets no open session, and nothing is written", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		bobs := bob()
		bobs.session = "bob-session"
		fixture.connectObserved(t, bobs)

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "deny", Session: "bob-session", Servers: []string{"github"}})
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, "You have no open session `bob-session`.", body)
		assert.Empty(t, fixture.scopeEntryRows(t))
	})

	t.Run("a deny with the default all and no list inserts a deny entry and refuses the server", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "all"})

		client := fixture.connectObserved(t, attestedAlice())
		assertNotBlocked(t, listServers(t, client.session), "github")

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})

		assert.Equal(t, []string{aliceWorkspace + " github  deny"}, fixture.scopeEntryRows(t))
		assertDenied(t, listServers(t, client.session), "github", "denied in this workspace")
		assert.Empty(t, api.lists(t, attestedAlice())[aliceWorkspace], "no list is made")
	})

	t.Run("a deny applies when the principal has 500 allow entries", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO scope_entries (scope_id, workspace, server, tool, effect)
			SELECT $1, 'coder/alice/other', 'github', 'tool_' || n, 'allow' FROM generate_series(1, 500) n`, fixture.userScopeID(t, "alice"))
		require.NoError(t, err)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github", "github__open_issue"}})

		entries := fixture.scopeEntryRows(t)
		assert.Len(t, entries, 502)
		assert.Contains(t, entries, aliceWorkspace+" github  deny")
		assert.Contains(t, entries, aliceWorkspace+" github create_issue deny")
	})

	t.Run("clear deletes the list and the allow entries and keeps the deny entries", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"slack"}})
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "list_issues", policy.EffectAllow)
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github__open_issue"}})

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "clear"})

		assert.Equal(t, []string{aliceWorkspace + " github create_issue deny"}, fixture.scopeEntryRows(t))

		listed := workspaceNamed(t, api.list(t, attestedAlice()), aliceWorkspace)
		assert.Nil(t, listed.Servers, "the list is deleted")
		assert.Equal(t, []string{"github__open_issue"}, listed.Denied)
	})

	t.Run("with the default all, clear widens through a change request and keeps the deny entries", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"slack"}})
		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "defaults", Verified: "all"})
		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github"}})

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "clear"})
		assert.Equal(t, []string{"slack"}, api.lists(t, attestedAlice())[aliceWorkspace], "nothing changes before the approval")

		require.NoError(t, api.approve(t, id))
		assert.Nil(t, workspaceNamed(t, api.list(t, attestedAlice()), aliceWorkspace).Servers)
		assert.Equal(t, []string{aliceWorkspace + " github  deny"}, fixture.scopeEntryRows(t))

		client := fixture.connectObserved(t, attestedAlice())
		assertDenied(t, listServers(t, client.session), "github", "denied in this workspace")
	})

	t.Run("a deny item with an empty tool is refused", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github__"}})
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "A tool name has 1 to 128 characters.", body)
		assert.Empty(t, fixture.scopeEntryRows(t))
	})

	t.Run("an unauthenticated deny is refused and writes nothing", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		status, _ := api.do(t, nil, http.MethodPost, workspacesapi.ChangesPath, workspacesapi.Change{Action: "deny", Servers: []string{"github"}})
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Empty(t, fixture.scopeEntryRows(t))
	})
}
