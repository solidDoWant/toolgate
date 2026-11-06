//go:build integration

package gateway_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// approveScope approves a scope request of alice's through the function
// the approval page calls.
func (a *workspaceAPI) approveScope(t *testing.T, identity auth.Identity, id string) error {
	t.Helper()

	return pgx.BeginFunc(t.Context(), a.fixture.pool, func(tx pgx.Tx) error {
		return a.workspaces.ApproveScopeRequest(t.Context(), tx, identity, id)
	})
}

// accessRequestRow returns the kind, status and target session of a
// request.
func (f *mcpFixture) accessRequestRow(t *testing.T, id string) (string, string, *int64) {
	t.Helper()

	var (
		kind, status string
		sessionPK    *int64
	)

	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT kind, status, session_pk FROM access_requests WHERE id = $1`, id).
		Scan(&kind, &status, &sessionPK))

	return kind, status, sessionPK
}

func TestScopeRequests(t *testing.T) {
	t.Run("an allow without credentials is refused and writes nothing", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		status, _ := api.do(t, nil, http.MethodPost, workspacesapi.ChangesPath,
			workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Zero(t, fixture.accessRequestCount(t))
	})

	t.Run("an allow of a server the user has no access to is refused and writes nothing", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		template := fixture.addEnableTemplate("bobonly", "per-user", aliceAndBob)
		template.Spec.Access.Users = []string{"bob"}
		fixture.catalog.Set(template, nil)

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"bobonly__tool"}})
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, "You don't have access to server `bobonly`.", body)
		assert.Zero(t, fixture.accessRequestCount(t))
	})

	t.Run("row 200: an allow for a session answers 202 with a scope request and changes nothing before approval", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github__open_issue"}})

		kind, status, sessionPK := fixture.accessRequestRow(t, id)
		assert.Equal(t, "scope", kind)
		assert.Equal(t, "pending", status)
		require.NotNil(t, sessionPK)
		assert.Equal(t, session.ID, *sessionPK)

		request, err := api.workspaces.AccessRequest(t.Context(), "alice", id)
		require.NoError(t, err)

		change, err := gateway.DecodeScopeChange(request.Change)
		require.NoError(t, err)
		assert.Equal(t, gateway.ScopeChange{Workspace: aliceWorkspace, Session: "session-1",
			Items: []gateway.ScopeItem{{Server: "github", Tool: "create_issue"}}, AskedBy: "session-1"}, change)

		assert.Empty(t, fixture.scopeEntryRows(t))
		assert.Empty(t, fixture.sessionAdditions(t, session.ID))
		require.Len(t, fixture.auditRows(t, gateway.TypeAccessRequest), 1)
		assert.Equal(t, "scope", fixture.auditRows(t, gateway.TypeAccessRequest)[0].Details["kind"])

		again := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github__open_issue"}})
		assert.Equal(t, id, again, "the pending request with the same target and change is found")
		assert.Equal(t, 1, fixture.accessRequestCount(t))
	})

	t.Run("a widening change of phase 3's shape still creates a change request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})
		kind, _, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "change", kind)
	})

	t.Run("row 221: the change-request approval, which a 0.3 replica runs, refuses a scope row and changes nothing", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})
		require.ErrorIs(t, api.approve(t, id), gateway.ErrAccessRequestUndecidable)

		_, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "pending", status)
		assert.Empty(t, fixture.sessionAdditions(t, session.ID))
		assert.Empty(t, api.lists(t, attestedAlice()))
	})

	t.Run("another user can't approve the request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})

		bob := aliceIdentity()
		bob.Principal, bob.Email = "bob", "bob@example.com"
		require.ErrorIs(t, api.approveScope(t, bob, id), gateway.ErrAccessRequestUndecidable)

		_, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "pending", status)
		assert.Empty(t, fixture.scopeEntryRows(t))
	})

	t.Run("workspace, server: adds it to a new list and deletes the server's deny entries", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "", policy.EffectDeny)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectDeny)

		result, err := api.workspaces.RequestScope(t.Context(), aliceIdentity(), gateway.ScopeTarget{Workspace: aliceWorkspace}, []string{"github"}, "")
		require.NoError(t, err)
		assert.Len(t, fixture.scopeEntryRows(t), 2, "nothing changes before approval")
		assert.Empty(t, api.lists(t, attestedAlice()))

		require.NoError(t, api.approveScope(t, aliceIdentity(), result.Request))

		assert.Equal(t, map[string][]string{aliceWorkspace: {"github"}}, api.lists(t, attestedAlice()))
		assert.Empty(t, fixture.scopeEntryRows(t))

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		require.Len(t, changes, 1)
		assert.Equal(t, result.Request, changes[0].Details["via"])
		assert.Equal(t, []any{"github"}, changes[0].Details["added"])
		require.Len(t, fixture.auditRows(t, gateway.TypeAccessApprove), 1)
		assert.Equal(t, "approved", api.requestStatus(t, attestedAlice(), result.Request))
	})

	t.Run("workspace, tool: deletes its deny entry and inserts an allow entry when the server isn't reachable", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectDeny)
		fixture.setVerifiedDefault(t, gateway.ScopeNone)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})
		assert.Equal(t, []string{aliceWorkspace + " github create_issue deny"}, fixture.scopeEntryRows(t))

		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		assert.Equal(t, []string{aliceWorkspace + " github create_issue allow"}, fixture.scopeEntryRows(t))
		assert.Empty(t, api.lists(t, attestedAlice()), "a tool allow creates no list")

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		require.Len(t, changes, 1)
		assert.Equal(t, id, changes[0].Details["via"])
		assert.Equal(t, []any{"github__open_issue"}, changes[0].Details["tools"])
	})

	t.Run("workspace, tool: inserts no allow entry when the list already reaches the server", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github"}})
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectDeny)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})
		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		assert.Empty(t, fixture.scopeEntryRows(t))
	})

	t.Run("session, server: inserts a session addition and deletes the session's deny entries for it", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")
		fixture.addSessionEntry(t, "session-1", "github", "", policy.EffectDeny)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "", policy.EffectDeny)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})
		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		assert.Equal(t, []string{"github"}, fixture.sessionAdditions(t, session.ID))
		assert.Equal(t, []string{aliceWorkspace + " github  deny"}, fixture.scopeEntryRows(t), "the workspace's deny stays")

		events := fixture.auditRows(t, gateway.TypeSessionAddition)
		require.Len(t, events, 1)
		assert.Equal(t, "github", events[0].Server)
		assert.Equal(t, "session-1", events[0].Session)
		assert.Equal(t, id, events[0].Details["via"])
		require.Len(t, fixture.auditRows(t, gateway.TypeAccessApprove), 1)
	})

	t.Run("session, tool: deletes the session's deny entry and inserts a session allow entry", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		fixture.addSessionEntry(t, "session-1", "github", "create_issue", policy.EffectDeny)
		fixture.setVerifiedDefault(t, gateway.ScopeNone)

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github__open_issue"}})
		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		assert.Equal(t, []string{"session:session-1 github create_issue allow"}, fixture.scopeEntryRows(t))
		require.Len(t, fixture.auditRows(t, gateway.TypeSessionAddition), 1)
	})

	t.Run("row 249: a tool of a server denied on the target is refused and nothing is written", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "", policy.EffectDeny)

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "Server `github` is denied in workspace `coder/alice/dev`. Allow the server first.", body)
		assert.Zero(t, fixture.accessRequestCount(t))
	})

	t.Run("an allow for an unverified session is refused", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, alice())

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "Session `session-1` has no verified workspace, so it uses only your default for unverified sessions.", body)
		assert.Zero(t, fixture.accessRequestCount(t))
	})

	t.Run("row 202: a session target in another workspace rolls the approval back with the reason", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})

		_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET workspace = 'coder/alice/other' WHERE id = $1`, session.ID)
		require.NoError(t, err)

		err = api.approveScope(t, aliceIdentity(), id)

		var refusal *gateway.WorkspaceError
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, "This session is now in another workspace.", refusal.Message)

		_, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "pending", status)
		assert.Empty(t, fixture.sessionAdditions(t, session.ID))
		assert.Empty(t, fixture.auditRows(t, gateway.TypeAccessApprove))
	})

	t.Run("row 201: ending the session expires its scope requests and deletes its allows, keeping its denies", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())
		session := fixture.sessionRow(t, "alice", "session-1")

		approved := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github", "github__open_issue"}})
		require.NoError(t, api.approveScope(t, aliceIdentity(), approved))
		fixture.addSessionEntry(t, "session-1", "github", "list_issues", policy.EffectAllow)
		fixture.addSessionEntry(t, "session-1", "github", "delete_repository", policy.EffectDeny)

		pending := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github__close_issue"}})

		require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, nil, session.ID, "closed"))

		_, status, _ := fixture.accessRequestRow(t, pending)
		assert.Equal(t, "expired", status)
		assert.Empty(t, fixture.sessionAdditions(t, session.ID))
		assert.Equal(t, []string{"session:session-1 github delete_repository deny"}, fixture.scopeEntryRows(t))

		expired := fixture.auditRows(t, "access.expire")
		require.Len(t, expired, 1)
		assert.Equal(t, pending, expired[0].Details["request"])
		assert.Equal(t, "scope", expired[0].Details["kind"])
	})

	t.Run("row 247: the leader's pass expires scope requests whose target session ended after the request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})
		workspaceRequest := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})

		// A 0.3 leader ends the session without touching scope requests.
		fixture.endSession(t)

		require.NoError(t, (&runtime.ApprovalExpirer{Pool: fixture.pool}).Run(t.Context()))

		_, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "expired", status)

		_, status, _ = fixture.accessRequestRow(t, workspaceRequest)
		assert.Equal(t, "pending", status, "a workspace target doesn't end with a session")

		expired := fixture.auditRows(t, "access.expire")
		require.Len(t, expired, 1)
		assert.Equal(t, id, expired[0].Details["request"])
		assert.Equal(t, "session-1", expired[0].Session)
	})

	t.Run("row 247: a scope request created after its session ended is left to its own expiry", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)
		fixture.connectObserved(t, attestedAlice())

		id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Session: "session-1", Servers: []string{"github"}})

		_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() - interval '1 minute' WHERE principal = 'alice' AND session_id = 'session-1'`)
		require.NoError(t, err)

		require.NoError(t, (&runtime.ApprovalExpirer{Pool: fixture.pool}).Run(t.Context()))

		_, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "pending", status)
	})

	t.Run("a scope request counts toward the 100 pending requests", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		curatedGithub(fixture)

		for index := range gateway.MaxPendingAccessRequestsPerPrincipal - 1 {
			_, err := fixture.pool.Exec(t.Context(), `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
				VALUES (lpad(to_hex($1::int), 32, '0'), 'alice', 'change', 'w', true, '{}', now() + interval '1 hour')`, index)
			require.NoError(t, err)
		}

		api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}})

		status, body := api.change(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github__list_issues"}})
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "You already have 100 pending access requests. Approve or deny some first.", body)
	})
}
