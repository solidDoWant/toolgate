//go:build integration

package gateway_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// endSession ends alice's session-1 as the idle pass does: ended_at is set
// and the enabled set stays.
func (f *mcpFixture) endSession(t *testing.T) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE principal = 'alice' AND session_id = 'session-1'")
	require.NoError(t, err)
}

// stopInstance marks an instance stopped, as the reconciler does once its
// sessions ended.
func (f *mcpFixture) stopInstance(t *testing.T, id string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET status = 'stopped' WHERE id = $1", id)
	require.NoError(t, err)
}

func TestSessionResume(t *testing.T) {
	t.Run("row 166: a resume from the same workspace restores the set, starts an instance without an enable and sends the tools with list_changed", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		client := alice()
		client.workspace = "repo-a"

		first := fixture.connectObserved(t, client)
		before := fixture.enableWithTools(t, first, "github", "session:alice:session-1", "echo")

		fixture.endSession(t)
		fixture.stopInstance(t, before.ID)

		resumed := fixture.connectObserved(t, client)

		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))

		instance := fixture.awaitInstance(t, template, "session:alice:session-1")
		assert.NotEqual(t, before.ID, instance.ID, "the resume inserts a new instance")
		assert.Equal(t, "pending", instance.Status)

		// Before the instance is ready, a call waits for it.
		early := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})
		fixture.assertWaiting(t, resumed, early)

		fixture.markReady(t, instance.ID, "echo")

		result := awaitWaitingCall(t, early)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "early", resultText(result))

		require.Eventually(t, func() bool { return resumed.listChangedCount() > 0 }, waitTimeout, 10*time.Millisecond)
		assert.Contains(t, toolNames(t, resumed.session), "github__echo")

		result = callTool(t, resumed.session, "github__echo", map[string]any{"text": "hi"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hi", resultText(result))

		resumes := fixture.auditRows(t, "session.resume")
		require.Len(t, resumes, 1)
		assert.Equal(t, []any{"github"}, resumes[0].Details["servers"])
		assert.Equal(t, "claimed/repo-a", resumes[0].Workspace)
		assert.Len(t, fixture.auditRows(t, "session.start"), 1, "a resume writes session.resume, not session.start")

		enables := fixture.auditEvents(t, "server.enable", "github")
		require.Len(t, enables, 2)
		assert.Equal(t, "allow", enables[1]["decision"])
		assert.Equal(t, true, enables[1]["resume"])
		assert.Equal(t, instance.ID, enables[1]["instance"])
	})

	t.Run("row 167: a resume from another workspace starts with no servers and writes session.start", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		client := alice()
		client.workspace = "repo-a"

		first := fixture.connectObserved(t, client)
		before := fixture.enableWithTools(t, first, "github", "session:alice:session-1", "echo")

		fixture.endSession(t)
		fixture.stopInstance(t, before.ID)

		client.workspace = "repo-b"
		fixture.connectObserved(t, client)

		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.instancesOf(t, template), "nothing starts for a session that restored nothing")
		assert.Empty(t, fixture.auditRows(t, "session.resume"))
		assert.Len(t, fixture.auditRows(t, "session.start"), 2)
	})

	t.Run("row 168: a rule added during the pause drops its server with server.disable; the server gets no tools and no call; the rest are restored", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.addEnableTemplate("jira", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")
		fixture.startUpstream(t, "jira")

		first := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, first, "github", "user:alice", "slow")
		fixture.enableWithTools(t, first, "jira", "user:alice", "echo")

		fixture.endSession(t)
		require.NoError(t, fixture.rules.Set(blockServer("github")))

		resumed := fixture.connectObserved(t, alice())

		assert.Equal(t, []string{"jira"}, fixture.enabledServers(t, "alice", "session-1"))

		disables := fixture.auditEvents(t, "server.disable", "github")
		require.Len(t, disables, 1)
		assert.Equal(t, "refuse", disables[0]["decision"])
		assert.Equal(t, "rule", disables[0]["step"])

		names := toolNames(t, resumed.session)
		assert.NotContains(t, names, "github__slow")
		assert.Contains(t, names, "jira__echo")

		result := callTool(t, resumed.session, "github__slow", nil)
		assert.True(t, result.IsError)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "rule", decisions[0].Details["step"], "the call is refused by the rule, so the server stays dropped")
		assert.Equal(t, "Refused by rule toolgate-servers/block-github: GitHub is frozen.", decisions[0].Details["reason"])
		assert.Equal(t, []string{"jira"}, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
		assert.Empty(t, upstream.started, "the call reached the upstream server")
	})

	t.Run("row 168: a workspace scope narrowed during the pause drops the server with server.disable", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		first := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, first, "github", "user:alice", "echo")

		fixture.endSession(t)
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)

		resumed := fixture.connectObserved(t, alice())

		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.NotContains(t, toolNames(t, resumed.session), "github__echo")

		disables := fixture.auditEvents(t, "server.disable", "github")
		require.Len(t, disables, 1)
		assert.Equal(t, "refuse", disables[0]["decision"])
		assert.Equal(t, unverifiedRefusal("github"), disables[0]["reason"])
	})

	t.Run("row 170: a server over the instance limit is dropped with server.disable, reason limit; the others are restored", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(1, 200))
		github := fixture.addEnableTemplate("github", "per-user", anyone)
		jira := fixture.addEnableTemplate("jira", "per-user", anyone)

		first := fixture.connectObserved(t, alice())
		fixture.enableReady(t, first, "github", "user:alice")

		session := fixture.sessionRow(t, "alice", "session-1")
		_, err := fixture.pool.Exec(t.Context(),
			"INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, $2, 'jira')", session.ID, templateNamespace)
		require.NoError(t, err)

		fixture.endSession(t)
		fixture.stopInstance(t, fixture.instancesOf(t, github)[0].ID)

		fixture.connectObserved(t, alice())

		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))
		require.Len(t, fixture.instancesOf(t, github), 1)
		assert.Empty(t, fixture.instancesOf(t, jira))

		disables := fixture.auditEvents(t, "server.disable", "jira")
		require.Len(t, disables, 1)
		assert.Equal(t, "refuse", disables[0]["decision"])
		assert.Equal(t, "limit", disables[0]["reason"])
		assert.Equal(t, "per-user", disables[0]["limit"])
	})

	t.Run("a session whose enabled set was forgotten restarts empty", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)

		first := fixture.connectObserved(t, alice())
		fixture.enableReady(t, first, "github", "session:alice:session-1")

		fixture.endSession(t)
		fixture.stopInstance(t, fixture.instancesOf(t, template)[0].ID)

		_, err := fixture.pool.Exec(t.Context(), "DELETE FROM session_servers")
		require.NoError(t, err)

		fixture.connectObserved(t, alice())

		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.instancesOf(t, template))
		assert.Empty(t, fixture.auditRows(t, "session.resume"))
	})
}

func TestSessionResumeInterrupted(t *testing.T) {
	t.Run("row 166: a restore that fails part-way leaves the session ended, and the next request restores every server once", func(t *testing.T) {
		fixture := newMCPFixture(t)
		github := fixture.addEnableTemplate("github", "per-session", anyone)
		jira := fixture.addEnableTemplate("jira", "per-session", anyone)
		fixture.startUpstream(t, "github")
		fixture.startUpstream(t, "jira")

		first := fixture.connectObserved(t, alice())
		githubBefore := fixture.enableWithTools(t, first, "github", "session:alice:session-1", "echo")
		jiraBefore := fixture.enableWithTools(t, first, "jira", "session:alice:session-1", "echo")

		fixture.endSession(t)
		fixture.stopInstance(t, githubBefore.ID)
		fixture.stopInstance(t, jiraBefore.ID)

		// The first restore inserts github's instance, then jira's insert
		// fails once, as a transient database error would.
		for _, statement := range []string{
			`CREATE TABLE restore_fail_once (x int)`,
			`INSERT INTO restore_fail_once VALUES (1)`,
			`CREATE FUNCTION restore_fail() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN
			   IF NEW.template_name = 'jira' AND EXISTS (SELECT 1 FROM restore_fail_once) THEN
			     RAISE EXCEPTION 'injected transient failure';
			   END IF;
			   RETURN NEW;
			 END $$`,
			`CREATE TRIGGER restore_fail BEFORE INSERT ON instances FOR EACH ROW EXECUTE FUNCTION restore_fail()`,
		} {
			_, err := fixture.pool.Exec(t.Context(), statement)
			require.NoError(t, err)
		}

		response := fixture.post(t, alice(), "", initializeMessage())
		require.NoError(t, response.Body.Close())
		assert.NotEqual(t, http.StatusOK, response.StatusCode)

		_, err := fixture.pool.Exec(t.Context(), `DELETE FROM restore_fail_once`)
		require.NoError(t, err)

		fixture.connectObserved(t, alice())

		assert.Equal(t, []string{"github", "jira"}, fixture.enabledServers(t, "alice", "session-1"))

		for _, template := range []*v1alpha1.MCPServerTemplate{github, jira} {
			live := fixture.instancesOf(t, template)
			require.Len(t, live, 1, "%s has one live instance", template.Name)

			enables := fixture.auditEvents(t, "server.enable", template.Name)
			require.Len(t, enables, 2, "%s: the enable and one restore decision", template.Name)
			assert.Equal(t, true, enables[1]["resume"])
			assert.Equal(t, live[0].ID, enables[1]["instance"])
		}

		resumes := fixture.auditRows(t, "session.resume")
		require.Len(t, resumes, 1)
		assert.Equal(t, []any{"github", "jira"}, resumes[0].Details["servers"])
	})
}
