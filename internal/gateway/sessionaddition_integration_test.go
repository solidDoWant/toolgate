//go:build integration

package gateway_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// serverRequest is one access_requests row of kind server.
type serverRequest struct {
	ID, Status, Workspace, Server string
	SessionPK                     int64
	Verified                      bool
	DecidedScope                  *string
}

func (f *mcpFixture) serverRequests(t *testing.T) []serverRequest {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), `SELECT id, status, workspace, server, session_pk, verified, decided_scope
		FROM access_requests WHERE kind = 'server' ORDER BY created_at, id`)
	require.NoError(t, err)

	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (serverRequest, error) {
		var request serverRequest

		err := row.Scan(&request.ID, &request.Status, &request.Workspace, &request.Server, &request.SessionPK, &request.Verified, &request.DecidedScope)

		return request, err
	})
	require.NoError(t, err)

	return requests
}

// awaitServerRequests waits until count server requests exist and returns
// them.
func (f *mcpFixture) awaitServerRequests(t *testing.T, count int) []serverRequest {
	t.Helper()

	var requests []serverRequest

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		requests = f.serverRequests(t)
		assert.Len(collect, requests, count)
	}, waitTimeout, 20*time.Millisecond)

	return requests
}

// approveServerRequest approves alice's server request with choice
// through the function the approval page calls.
func approveServerRequest(t *testing.T, fixture *mcpFixture, id, choice string) error {
	t.Helper()

	workspaces := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0)

	return pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
		return workspaces.ApproveServerRequest(t.Context(), tx, aliceIdentity(), id, choice)
	})
}

// endIdleSession ends alice's session through the reconciler's session
// ender, as after the idle timeout.
func (f *mcpFixture) endIdleSession(t *testing.T, session string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `UPDATE sessions SET last_seen_at = now() - interval '3 hours'
		WHERE principal = 'alice' AND session_id = $1`, session)
	require.NoError(t, err)

	ender := &runtime.SessionEnder{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}
	require.NoError(t, ender.Run(t.Context()))
	require.True(t, f.sessionRow(t, "alice", session).EndedAt.Valid)
}

func pendingAdditionMessage(server, id string) string {
	return fmt.Sprintf("Server `%s` isn't allowed in workspace `%s`. Allow it at %s%s, then enable it again.",
		server, aliceWorkspace, approvalBaseURL, id)
}

func newAdditionFixture(t *testing.T) *mcpFixture {
	t.Helper()

	fixture := newMCPFixtureWithAuth(t, attestedUsers(), withWorkspaceScoping, withApprovalURL)
	fixture.addEnableTemplate("github", "per-user", aliceAndBob)
	fixture.startUpstream(t, "github")

	return fixture
}

func attestedAliceIn(session string) user {
	client := attestedAlice()
	client.session = session

	return client
}

func TestSessionAdditions(t *testing.T) {
	t.Run("row 131, row 137: without elicitation an enable inserts one server request, fails with the link, and finds the same request again", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectObserved(t, attestedAlice())

		assertBlockedInWorkspace(t, listServers(t, client.session), "github")

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)

		requests := fixture.serverRequests(t)
		require.Len(t, requests, 1)

		request := requests[0]
		assert.Equal(t, pendingAdditionMessage("github", request.ID), resultText(result))
		assert.Equal(t, "pending", request.Status)
		assert.Equal(t, aliceWorkspace, request.Workspace)
		assert.Equal(t, "github", request.Server)
		assert.True(t, request.Verified)
		assert.Equal(t, fixture.sessionRow(t, "alice", "session-1").ID, request.SessionPK)

		again := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, again.IsError)
		assert.Equal(t, pendingAdditionMessage("github", request.ID), resultText(again))
		assert.Len(t, fixture.serverRequests(t), 1, "the retry finds the pending request")

		accessRequests := fixture.auditRows(t, gateway.TypeAccessRequest)
		require.Len(t, accessRequests, 1)
		assert.Equal(t, request.ID, accessRequests[0].Details["request"])

		enables := fixture.auditRows(t, "server.enable")
		require.Len(t, enables, 2)

		for _, enable := range enables {
			assert.Equal(t, "refuse", enable.Details["decision"])
			assert.Equal(t, policy.StepWorkspaceScope, enable.Details["step"])
			assert.Equal(t, request.ID, enable.Details["request"])
		}

		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("row 132, row 136: a session approval lets the next enable through in that session only", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		first := fixture.connectObserved(t, attestedAlice())

		require.True(t, callTool(t, first.session, gateway.ToolEnableServer, map[string]any{"name": "github"}).IsError)

		request := fixture.serverRequests(t)[0]
		require.NoError(t, approveServerRequest(t, fixture, request.ID, gateway.AdditionSession))

		approved := fixture.serverRequests(t)[0]
		assert.Equal(t, "approved", approved.Status)
		require.NotNil(t, approved.DecidedScope)
		assert.Equal(t, gateway.AdditionSession, *approved.DecidedScope)
		assert.Equal(t, []string{"github"}, fixture.sessionAdditions(t, request.SessionPK))

		additions := fixture.auditRows(t, gateway.TypeSessionAddition)
		require.Len(t, additions, 1)
		assert.Equal(t, request.ID, additions[0].Details["request"])
		assert.Equal(t, "session-1", additions[0].Session)
		assert.Equal(t, "github", additions[0].Server)
		require.Len(t, fixture.auditRows(t, gateway.TypeAccessApprove), 1)

		fixture.enableWithTools(t, first, "github", "user:alice", "echo")

		second := fixture.connectObserved(t, attestedAliceIn("session-2"))
		assertBlockedInWorkspace(t, listServers(t, second.session), "github")

		result := callTool(t, second.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError, "the other session doesn't get the addition")
		assert.Len(t, fixture.serverRequests(t), 2)
	})

	t.Run("row 133: an \"always\" approval wakes the enables waiting in two sessions of the workspace", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		first := fixture.connectElicitingAs(t, attestedAlice(), elicitingOptions{revision: revisionElicitCreate, answer: "accept"})
		second := fixture.connectElicitingAs(t, attestedAliceIn("session-2"), elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		firstResults := startEnable(t, first.session, "github")
		fixture.awaitWaiting(t, first)

		secondResults := startEnable(t, second.session, "github")
		fixture.awaitWaiting(t, second)

		requests := fixture.awaitServerRequests(t, 2)
		require.Len(t, first.elicitations(), 1)
		assert.Equal(t, approvalBaseURL+requests[0].ID, first.elicitations()[0].URL)

		started := time.Now()

		require.NoError(t, approveServerRequest(t, fixture, requests[0].ID, gateway.AdditionWorkspace))

		// Both waits wake on the notifications, before the 10 s tick, and
		// go on to enable the server.
		require.Eventually(t, func() bool {
			return len(fixture.enabledServers(t, "alice", "session-1")) == 1 && len(fixture.enabledServers(t, "alice", "session-2")) == 1
		}, approvalWakeBound, 20*time.Millisecond, "both waits woke")
		assert.Less(t, time.Since(started), approvalWakeBound)

		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "user:alice")
		fixture.markReady(t, instance.ID, "echo")

		assert.Empty(t, awaitEnable(t, firstResults).err)
		assert.Empty(t, awaitEnable(t, secondResults).err)

		after := fixture.serverRequests(t)
		assert.Equal(t, "approved", after[0].Status)
		assert.Equal(t, "pending", after[1].Status, "the other session's request stays pending")

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		require.Len(t, changes, 1)
		assert.Equal(t, requests[0].ID, changes[0].Details["via"])
		assert.Equal(t, []any{"github"}, changes[0].Details["added"])

		allows := 0

		for _, enable := range fixture.auditRows(t, "server.enable") {
			if enable.Details["decision"] == "allow" {
				allows++

				assert.NotEmpty(t, enable.Details["request"])
			}
		}

		assert.Equal(t, 2, allows)
	})

	t.Run("row 157: the session ending during a wait stops the enable without restarting the session", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectElicitingAs(t, attestedAlice(), elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		results := startEnable(t, client.session, "github")
		fixture.awaitWaiting(t, client)

		request := fixture.awaitServerRequests(t, 1)[0]

		fixture.endIdleSession(t, "session-1")

		outcome := awaitEnable(t, results)
		assert.Equal(t, "This session ended while waiting for `github`.", outcome.err)
		assert.True(t, fixture.sessionRow(t, "alice", "session-1").EndedAt.Valid, "the session stays ended")
		assert.Equal(t, "expired", fixture.serverRequests(t)[0].Status)

		var reasons []any

		for _, enable := range fixture.auditRows(t, "server.enable") {
			reasons = append(reasons, enable.Details["reason"])

			assert.Equal(t, request.ID, enable.Details["request"])
		}

		assert.Equal(t, []any{"session-ended"}, reasons)
	})

	t.Run("row 134, row 135, row 154: ending a session deletes its additions and expires its requests, so a restarted session has none", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)
		client := fixture.connectObserved(t, attestedAlice())

		require.True(t, callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"}).IsError)
		require.True(t, callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "slack"}).IsError)

		requests := fixture.serverRequests(t)
		require.Len(t, requests, 2)
		require.NoError(t, approveServerRequest(t, fixture, requests[0].ID, gateway.AdditionSession))
		require.Equal(t, []string{"github"}, fixture.sessionAdditions(t, requests[0].SessionPK))

		fixture.endIdleSession(t, "session-1")

		assert.Empty(t, fixture.sessionAdditions(t, requests[0].SessionPK))
		assert.Equal(t, "expired", fixture.serverRequests(t)[1].Status)

		expired := fixture.auditRows(t, "access.expire")
		require.Len(t, expired, 1)
		assert.Equal(t, requests[1].ID, expired[0].Details["request"])
		assert.Equal(t, "session-1", expired[0].Session)

		err := approveServerRequest(t, fixture, requests[1].ID, gateway.AdditionSession)
		require.ErrorIs(t, err, gateway.ErrAccessRequestUndecidable, "the expired request offers nothing")

		restarted := fixture.connectObserved(t, attestedAlice())
		assertBlockedInWorkspace(t, listServers(t, restarted.session), "github")

		result := callTool(t, restarted.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assert.Len(t, fixture.serverRequests(t), 3, "the restarted session asks again")
	})

	t.Run("row 154: a session approval refuses a session that ended or moved to another workspace", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectObserved(t, attestedAlice())

		require.True(t, callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"}).IsError)

		request := fixture.serverRequests(t)[0]

		_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() WHERE id = $1`, request.SessionPK)
		require.NoError(t, err)

		var refusal *gateway.WorkspaceError

		err = approveServerRequest(t, fixture, request.ID, gateway.AdditionSession)
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "This session has ended.", refusal.Message)

		_, err = fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = NULL, workspace = 'coder/alice/other' WHERE id = $1`, request.SessionPK)
		require.NoError(t, err)

		err = approveServerRequest(t, fixture, request.ID, gateway.AdditionSession)
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "This session is now in another workspace.", refusal.Message)

		_, err = fixture.pool.Exec(t.Context(), `UPDATE sessions SET workspace = $2, attestation = 'user' WHERE id = $1`, request.SessionPK, aliceWorkspace)
		require.NoError(t, err)

		err = approveServerRequest(t, fixture, request.ID, gateway.AdditionSession)
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "This session is now in another workspace.", refusal.Message)

		assert.Equal(t, "pending", fixture.serverRequests(t)[0].Status, "each refusal rolls back")
		assert.Empty(t, fixture.sessionAdditions(t, request.SessionPK))
		assert.Empty(t, fixture.auditRows(t, gateway.TypeAccessApprove))
	})
}

// insertServerRequest inserts a pending server request of alice's for
// server in the session, as an enable does.
func (f *mcpFixture) insertServerRequest(t *testing.T, sessionPK int64, server string) string {
	t.Helper()

	var id string
	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO access_requests (id, principal, kind, session_pk, workspace, verified, server, expires_at)
		VALUES (md5(random()::text), 'alice', 'server', $1, $2, true, $3, now() + interval '10 minutes') RETURNING id`,
		sessionPK, aliceWorkspace, server).Scan(&id))

	return id
}

// openAttestedSession opens alice's session-1 in her verified workspace and
// returns its key.
func (f *mcpFixture) openAttestedSession(t *testing.T) int64 {
	t.Helper()

	f.connectObserved(t, attestedAlice())

	return f.sessionRow(t, "alice", "session-1").ID
}

// fillList gives alice's list for the workspace count servers.
func (f *mcpFixture) fillList(t *testing.T, workspace string, count int) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `WITH scope AS (
			INSERT INTO user_scopes (principal) VALUES ('alice')
			ON CONFLICT (principal) DO UPDATE SET principal = EXCLUDED.principal RETURNING id),
		list AS (INSERT INTO workspace_lists (scope_id, workspace) SELECT id, $1 FROM scope RETURNING id)
		INSERT INTO workspace_list_servers (list_id, server)
		SELECT list.id, 'filler-' || n FROM list, generate_series(1, $2::int) n`, workspace, count)
	require.NoError(t, err)
}

func (f *mcpFixture) requestStatus(t *testing.T, id string) string {
	t.Helper()

	var status string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT status FROM access_requests WHERE id = $1`, id).Scan(&status))

	return status
}

func TestServerRequestApprovalLimits(t *testing.T) {
	limitError := func(message string) require.ErrorAssertionFunc {
		return func(t require.TestingT, err error, _ ...any) {
			var refusal *gateway.WorkspaceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, message, refusal.Message)
		}
	}

	tests := []struct {
		name    string
		choice  string
		prepare func(t *testing.T, fixture *mcpFixture, sessionPK int64)
		check   require.ErrorAssertionFunc
	}{
		{
			name: "row 160: a session approval at 31 additions succeeds", choice: gateway.AdditionSession,
			prepare: func(t *testing.T, fixture *mcpFixture, sessionPK int64) {
				for index := range gateway.MaxSessionAdditions - 1 {
					fixture.addSessionAddition(t, sessionPK, fmt.Sprintf("filler-%d", index))
				}
			},
		},
		{
			name: "row 160: a session approval at 32 additions rolls back", choice: gateway.AdditionSession,
			prepare: func(t *testing.T, fixture *mcpFixture, sessionPK int64) {
				for index := range gateway.MaxSessionAdditions {
					fixture.addSessionAddition(t, sessionPK, fmt.Sprintf("filler-%d", index))
				}
			},
			check: limitError("This session already has 32 added servers. Remove one, then approve again."),
		},
		{
			name: "row 163: a session approval keeps an existing addition and succeeds", choice: gateway.AdditionSession,
			prepare: func(t *testing.T, fixture *mcpFixture, sessionPK int64) {
				for index := range gateway.MaxSessionAdditions - 1 {
					fixture.addSessionAddition(t, sessionPK, fmt.Sprintf("filler-%d", index))
				}

				fixture.addSessionAddition(t, sessionPK, "github")
			},
		},
		{
			name: "row 160: an \"always\" approval at 63 servers in the list succeeds", choice: gateway.AdditionWorkspace,
			prepare: func(t *testing.T, fixture *mcpFixture, _ int64) {
				fixture.fillList(t, aliceWorkspace, gateway.MaxWorkspaceListServers-1)
			},
		},
		{
			name: "row 160: an \"always\" approval at 64 servers in the list rolls back", choice: gateway.AdditionWorkspace,
			prepare: func(t *testing.T, fixture *mcpFixture, _ int64) {
				fixture.fillList(t, aliceWorkspace, gateway.MaxWorkspaceListServers)
			},
			check: limitError("Workspace `coder/alice/dev`'s list already has 64 servers. Remove one, then approve again."),
		},
		{
			name: "row 160: an \"always\" approval at 199 other lists succeeds", choice: gateway.AdditionWorkspace,
			prepare: func(t *testing.T, fixture *mcpFixture, _ int64) {
				for index := range gateway.MaxWorkspaceLists - 1 {
					fixture.fillList(t, fmt.Sprintf("coder/alice/w%d", index), 0)
				}
			},
		},
		{
			name: "row 160: an \"always\" approval at 200 other lists rolls back", choice: gateway.AdditionWorkspace,
			prepare: func(t *testing.T, fixture *mcpFixture, _ int64) {
				for index := range gateway.MaxWorkspaceLists {
					fixture.fillList(t, fmt.Sprintf("coder/alice/w%d", index), 0)
				}
			},
			check: limitError("You already have 200 workspace lists. Remove one, then approve again."),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdditionFixture(t)
			sessionPK := fixture.openAttestedSession(t)
			test.prepare(t, fixture, sessionPK)
			id := fixture.insertServerRequest(t, sessionPK, "github")

			check := test.check
			if check == nil {
				check = require.NoError
			}

			err := approveServerRequest(t, fixture, id, test.choice)
			check(t, err)

			if test.check != nil {
				assert.Equal(t, "pending", fixture.requestStatus(t, id))
				assert.Empty(t, fixture.auditRows(t, gateway.TypeAccessApprove))

				return
			}

			assert.Equal(t, "approved", fixture.requestStatus(t, id))

			if test.choice == gateway.AdditionSession {
				assert.Contains(t, fixture.sessionAdditions(t, sessionPK), "github")
				assert.Len(t, fixture.auditRows(t, gateway.TypeSessionAddition), 1)
			} else {
				servers, err := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0).Scopes(t.Context(), "alice")
				require.NoError(t, err)

				found := false

				for _, list := range servers.Lists {
					if list.Workspace == aliceWorkspace {
						found = assert.Contains(t, list.Servers, "github")
					}
				}

				assert.True(t, found, "the workspace has a list with the server")
			}
		})
	}

	t.Run("row 161: a deny that commits after a session approval deletes the addition", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		sessionPK := fixture.openAttestedSession(t)
		fixture.fillList(t, aliceWorkspace, 1)

		id := fixture.insertServerRequest(t, sessionPK, "github")
		require.NoError(t, approveServerRequest(t, fixture, id, gateway.AdditionSession))
		require.Equal(t, []string{"github"}, fixture.sessionAdditions(t, sessionPK))

		workspaces := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0)
		require.NoError(t, workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceDeny, Workspace: aliceWorkspace, Servers: []string{"github"}}, gateway.ViaUI))

		assert.Empty(t, fixture.sessionAdditions(t, sessionPK))
	})
}
