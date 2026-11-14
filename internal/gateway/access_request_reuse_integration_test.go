//go:build integration

package gateway_test

import (
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// pendingRequestCount is the number of alice's pending access requests.
func (f *mcpFixture) pendingRequestCount(t *testing.T) int {
	t.Helper()

	var count int

	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM access_requests WHERE principal = 'alice' AND status = 'pending'`).Scan(&count))

	return count
}

// workspaceAllow is an allow of servers on alice's verified workspace.
func workspaceAllow(servers ...string) workspacesapi.Change {
	return workspacesapi.Change{Action: "allow", Servers: servers}
}

// A repeated request for the same change waits on the pending one instead
// of adding another request for the user to decide.
func TestRepeatedAccessRequestReusesPendingOne(t *testing.T) {
	t.Run("two identical workspace allows give one pending request with one ID and URL", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		second := api.requestChange(t, attestedAlice(), workspaceAllow("github"))

		assert.Equal(t, first, second)
		assert.Equal(t, 1, fixture.pendingRequestCount(t))
		assert.Len(t, fixture.auditRows(t, gateway.TypeAccessRequest), 1, "a reused request writes no second access.request")
	})

	t.Run("the order of the servers doesn't matter", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github", "slack"))
		second := api.requestChange(t, attestedAlice(), workspaceAllow("slack", "github"))

		assert.Equal(t, first, second)
		assert.Equal(t, 1, fixture.pendingRequestCount(t))
	})

	t.Run("a different change gets its own request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		second := api.requestChange(t, attestedAlice(), workspaceAllow("github", "slack"))

		assert.NotEqual(t, first, second)
		assert.Equal(t, 2, fixture.pendingRequestCount(t))
	})

	t.Run("an unverified allow asked again from another session reuses the pending request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("jira", "per-user", aliceAndBob)

		other := unverifiedAlice()
		other.session = "unverified-2"

		first := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "jira", "github"))
		second := api.requestChange(t, other, unverifiedChange("allow", "github", "jira"))

		assert.Equal(t, first, second)
		assert.Equal(t, 1, fixture.pendingRequestCount(t))
		assert.Len(t, fixture.auditRows(t, gateway.TypeAccessRequest), 1)
	})

	t.Run("concurrent identical requests from two sessions give one request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		sessions := []string{"session-1", "session-2"}
		results := make([]gateway.WorkspaceResult, len(sessions))
		errs := make([]error, len(sessions))

		var group sync.WaitGroup

		for index, session := range sessions {
			group.Add(1)

			go func() {
				defer group.Done()

				identity := aliceIdentity()
				identity.Session = session
				results[index], errs[index] = api.workspaces.Request(t.Context(), identity,
					gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Servers: []string{"github"}})
			}()
		}

		group.Wait()

		for index := range sessions {
			require.NoError(t, errs[index])
			assert.False(t, results[index].Applied)
		}

		assert.Equal(t, results[0].Request, results[1].Request)
		assert.NotEqual(t, results[0].Inserted, results[1].Inserted, "exactly one call inserts the request")
		assert.Equal(t, 1, fixture.pendingRequestCount(t))
	})

	t.Run("a denied request isn't reused", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		require.NoError(t, api.deny(t, first))

		second := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		assert.NotEqual(t, first, second)
	})

	t.Run("after an approval the same change is already in effect and needs no request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		require.NoError(t, api.approve(t, first))

		api.applyChange(t, attestedAlice(), workspaceAllow("github"))
		assert.Zero(t, fixture.pendingRequestCount(t))

		id := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "github"))
		require.NoError(t, api.approveScope(t, aliceIdentity(), id))
		api.applyChange(t, unverifiedAlice(), unverifiedChange("allow", "github"))
		assert.Zero(t, fixture.pendingRequestCount(t))
	})

	t.Run("an expired request isn't reused", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		first := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		_, err := fixture.pool.Exec(t.Context(),
			`UPDATE access_requests SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE id = $1`, first)
		require.NoError(t, err)

		second := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		assert.NotEqual(t, first, second)
	})
}

// Approving one request closes the user's other pending requests that it
// made redundant, so the user isn't asked twice for one effect.
func TestApprovalClosesRequestsItMadeRedundant(t *testing.T) {
	t.Run("approving a scope allow closes a change request for the same servers", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		change := api.requestChange(t, attestedAlice(), workspaceAllow("github"))

		scope, err := api.workspaces.RequestScope(t.Context(), aliceIdentity(), gateway.ScopeTarget{Workspace: aliceWorkspace}, []string{"github"}, "")
		require.NoError(t, err)
		require.NotEqual(t, change, scope.Request)

		require.NoError(t, api.approveScope(t, aliceIdentity(), scope.Request))

		assert.Equal(t, "approved", api.requestStatus(t, attestedAlice(), change))
		assert.Zero(t, fixture.pendingRequestCount(t))

		approvals := fixture.auditRows(t, gateway.TypeAccessApprove)
		require.Len(t, approvals, 2)
		assert.Equal(t, change, approvals[1].Details["request"])
		assert.Equal(t, scope.Request, approvals[1].Details["satisfiedBy"])
	})

	t.Run("approving a change request closes a scope request it covers but not one it doesn't", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)

		covered, err := api.workspaces.RequestScope(t.Context(), aliceIdentity(), gateway.ScopeTarget{Workspace: aliceWorkspace}, []string{"github"}, "")
		require.NoError(t, err)

		uncovered, err := api.workspaces.RequestScope(t.Context(), aliceIdentity(), gateway.ScopeTarget{Workspace: aliceWorkspace}, []string{"slack"}, "")
		require.NoError(t, err)

		change := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		require.NoError(t, api.approve(t, change))

		assert.Equal(t, "approved", api.requestStatus(t, attestedAlice(), covered.Request))
		assert.Equal(t, "pending", api.requestStatus(t, attestedAlice(), uncovered.Request))
	})

	t.Run("approving one unverified allow closes another unverified allow it covers", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("jira", "per-user", aliceAndBob)

		narrow := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "github"))
		wide := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "github", "jira"))

		require.NoError(t, api.approveScope(t, aliceIdentity(), wide))

		assert.Equal(t, "approved", api.requestStatus(t, unverifiedAlice(), narrow))
		assert.Zero(t, fixture.pendingRequestCount(t))
	})

	t.Run("a denial closes nothing else", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		change := api.requestChange(t, attestedAlice(), workspaceAllow("github"))
		scope, err := api.workspaces.RequestScope(t.Context(), aliceIdentity(), gateway.ScopeTarget{Workspace: aliceWorkspace}, []string{"github"}, "")
		require.NoError(t, err)

		require.NoError(t, api.deny(t, scope.Request))
		assert.Equal(t, "pending", api.requestStatus(t, attestedAlice(), change))
	})
}

// deny denies an access request of alice's through the function the
// approval page calls.
func (a *workspaceAPI) deny(t *testing.T, id string) error {
	t.Helper()

	return pgx.BeginFunc(t.Context(), a.fixture.pool, func(tx pgx.Tx) error {
		return a.workspaces.DenyAccessRequest(t.Context(), tx, aliceIdentity(), id)
	})
}
