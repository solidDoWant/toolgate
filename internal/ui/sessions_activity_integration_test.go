//go:build integration

package ui_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertInstance stores an instance of a template in the "default" namespace.
func (e *uiEnv) insertInstance(t *testing.T, id, template, scope, scopeKey, owner, status string) {
	t.Helper()

	_, err := e.pool.Exec(t.Context(),
		`INSERT INTO instances (id, template_ns, template_name, template_uid, generation, scope, scope_key, owner_principal, status, namespace)
		 VALUES ($1, 'default', $2, 'uid', 1, $3, $4, $5, $6, 'ns')`,
		fmt.Sprintf("%032s", id), template, scope, scopeKey, owner, status)
	require.NoError(t, err)
}

func (e *uiEnv) insertDecisionWith(t *testing.T, principal, sessionID, decision string, at time.Time) {
	t.Helper()

	e.insertDecision(t, "tool.decision", principal, sessionID, "tool", at)

	_, err := e.pool.Exec(t.Context(),
		`UPDATE audit_events SET details = jsonb_build_object('decision', $1::text)
		 WHERE session = $2 AND principal = $3 AND occurred_at = $4`, decision, sessionID, principal, at)
	require.NoError(t, err)
}

func TestSessionsListShowsServersAndActivityOfEachSession(t *testing.T) {
	env := startUIEnv(t, "admins")
	user := env.insertSessionOf(t, "frank", "activity-user", nil)

	busy := env.insertListedSession(t, "dave", "busy-session", "ws", "user", "", 1, -1, false)
	quiet := env.insertListedSession(t, "dave", "quiet-session", "ws", "user", "", 2, -1, false)
	other := env.insertListedSession(t, "erin", "busy-session", "ws", "user", "", 1, -1, false)

	for _, server := range []string{"github", "slack"} {
		_, err := env.pool.Exec(t.Context(), `INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, 'default', $2)`, busy, server)
		require.NoError(t, err)
	}

	now := time.Now().UTC()
	env.insertDecisionWith(t, "dave", "busy-session", "allow", now.Add(-10*time.Minute))
	env.insertDecisionWith(t, "dave", "busy-session", "allow", now.Add(-20*time.Minute))
	env.insertDecisionWith(t, "dave", "busy-session", "deny", now.Add(-30*time.Minute))
	env.insertDecisionWith(t, "dave", "busy-session", "refuse", now.Add(-40*time.Minute))
	env.insertDecisionWith(t, "dave", "busy-session", "allow", now.Add(-90*time.Minute)) // outside the hour
	env.insertDecisionWith(t, "erin", "busy-session", "deny", now.Add(-5*time.Minute))   // another user's session ID

	// Pending approvals: one for busy, one expired, one decided.
	insert := func(id string, session int64, status, expires string) {
		_, err := env.pool.Exec(t.Context(),
			`INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, status, created_at, expires_at)
			 VALUES ($1, 'dave', $2, 'ws', 'default', 'github', 'tool', $3, $4, now() - interval '1 hour', now() + $5::interval)`,
			fmt.Sprintf("%032s", id), session, hashOf(id), status, expires)
		require.NoError(t, err)
	}
	insert("a1", busy, "pending", "5 minutes")
	insert("a2", busy, "pending", "-5 minutes")
	insert("a3", busy, "approved", "5 minutes")

	// Instances: shared and own per-session count; stopped, other user's, other session's do not.
	env.insertInstance(t, "1", "github", "shared", "shared", "", "ready")
	env.insertInstance(t, "2", "slack", "per-session", "session:dave:busy-session", "dave", "ready")
	env.insertInstance(t, "3", "other", "per-user", "dave", "dave", "ready") // not enabled by the session
	env.insertInstance(t, "4", "slack", "per-user", "erin", "erin", "ready") // another user's
	env.insertInstance(t, "5", "github", "per-session", "session:dave:quiet-session", "dave", "ready")
	env.insertInstance(t, "6", "github", "per-user", "dave", "dave", "starting")

	page := func(session *http.Client) string {
		response := get(t, session, env.server.URL+"/ui/sessions?state=all&user=all")
		require.Equal(t, http.StatusOK, response.StatusCode)

		return readBody(t, response)
	}

	admin := env.insertSession(t, "activity-admin", []string{"admins"})
	body := page(admin)

	t.Run("a busy session shows its servers, running instances and counts", func(t *testing.T) {
		row := rowFor(body, "/ui/sessions/"+itoa(busy)+`"`)
		assert.Contains(t, row, `badge-neutral">default/github</span>`)
		assert.Contains(t, row, `badge-neutral">default/slack</span>`)
		assert.Contains(t, row, "2 running")
		assert.Contains(t, row, ">4 calls</a>")
		assert.Contains(t, row, ">2 refused</a>")
		assert.Contains(t, row, ">1 pending</a>")
		assert.Contains(t, row, `href="/ui/audit?from=`)
		assert.Contains(t, row, "session=busy-session")
		assert.Contains(t, row, "principal=dave")
	})

	t.Run("a session with no activity shows zeros", func(t *testing.T) {
		row := rowFor(body, "/ui/sessions/"+itoa(quiet)+`"`)
		assert.Contains(t, row, "0 running")
		assert.Contains(t, row, ">0 calls</a>")
		assert.Contains(t, row, ">0 refused</a>")
		assert.Contains(t, row, ">0 pending</a>")
	})

	t.Run("another user's session with the same ID counts its own events only", func(t *testing.T) {
		row := rowFor(body, "/ui/sessions/"+itoa(other)+`"`)
		assert.Contains(t, row, ">1 calls</a>")
		assert.Contains(t, row, ">1 refused</a>")
		assert.Contains(t, row, "0 running")
		assert.Contains(t, row, ">0 pending</a>")
	})

	t.Run("a user's list leaves out other users' sessions", func(t *testing.T) {
		assert.NotContains(t, page(user), "/ui/sessions/"+itoa(busy)+`"`)
	})
}
