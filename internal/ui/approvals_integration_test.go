//go:build integration

package ui_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/ui"
)

const approvalTimeout = 10 * time.Minute

// insertApproval stores a pending approval of principal, with a session of
// its own, expiring in expiresIn (negative: already past).
func (e *uiEnv) insertApproval(t *testing.T, id, principal string, expiresIn time.Duration) {
	t.Helper()

	queries := db.New(e.pool)

	session, err := queries.CreateSession(t.Context(), db.CreateSessionParams{
		Principal: principal, SessionID: "session-" + id, Workspace: "github.com/acme/repo", Attestation: "user", Kind: "user", Groups: []string{},
	})
	require.NoError(t, err)

	_, err = e.pool.Exec(t.Context(),
		`INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, 'toolgate', 'github', 'delete_repository', $5, now() - interval '1 hour', now() + $6::interval)`,
		id, principal, session.ID, session.Workspace, hashOf(id),
		pgtype.Interval{Microseconds: expiresIn.Microseconds(), Valid: true})
	require.NoError(t, err)
}

func (e *uiEnv) approvalRow(t *testing.T, id string) db.Approval {
	t.Helper()

	row, err := db.New(e.pool).GetApproval(t.Context(), id)
	require.NoError(t, err)

	return row
}

// approvalEvents returns the events of type about approval id.
func (e *uiEnv) approvalEvents(t *testing.T, eventType, id string) []map[string]string {
	t.Helper()

	rows, err := e.pool.Query(t.Context(),
		`SELECT principal, workspace, session, attestation, server, tool FROM audit_events
		 WHERE type = $1 AND details->>'approval' = $2`, eventType, id)
	require.NoError(t, err)

	defer rows.Close()

	var events []map[string]string

	for rows.Next() {
		var principal, workspace, session, attestation, server, tool string
		require.NoError(t, rows.Scan(&principal, &workspace, &session, &attestation, &server, &tool))
		events = append(events, map[string]string{
			"principal": principal, "workspace": workspace, "session": session,
			"attestation": attestation, "server": server, "tool": tool,
		})
	}

	require.NoError(t, rows.Err())

	return events
}

// listen returns a function that waits for the next notification on the
// gateway's channel.
func (e *uiEnv) listen(t *testing.T) func() string {
	t.Helper()

	conn, err := e.pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)

	_, err = conn.Exec(t.Context(), "LISTEN "+store.Channel)
	require.NoError(t, err)

	return func() string {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		notification, err := conn.Conn().WaitForNotification(ctx)
		require.NoError(t, err)

		return notification.Payload
	}
}

func approvalID(n byte) string {
	return strings.Repeat(string(rune('a'+n)), 32)
}

func TestApprovalsAreOwnerOnly(t *testing.T) {
	env := startUIEnv(t, "admins")
	id := approvalID(0)
	env.insertApproval(t, id, "erin", approvalTimeout)

	admin := env.insertSession(t, "admin-session", []string{"admins"})
	env.ageSignIn(t, "admin-session", 0)

	token := csrfTokenOf("admin-session")

	detail := get(t, admin, env.server.URL+ui.ApprovalPath(id))
	assert.Equal(t, http.StatusNotFound, detail.StatusCode)

	list := get(t, admin, env.server.URL+ui.ApprovalsPath)
	require.Equal(t, http.StatusOK, list.StatusCode)
	assert.NotContains(t, readBody(t, list), id, "the list leaves out other principals' approvals")

	for _, action := range []string{"approve", "deny"} {
		response := post(t, admin, env.server.URL+ui.ApprovalPath(id)+"/"+action, url.Values{ui.CSRFField: {token}})
		assert.Equal(t, http.StatusNotFound, response.StatusCode, action)
	}

	assert.Equal(t, "pending", env.approvalRow(t, id).Status, "nobody but erin decides")
}

func TestApprovalsList(t *testing.T) {
	env := startUIEnv(t, "")
	env.insertApproval(t, approvalID(0), "dave", approvalTimeout)
	env.insertApproval(t, approvalID(1), "dave", -time.Second)
	browser := env.insertSession(t, "list-session", nil)

	response := get(t, browser, env.server.URL+ui.ApprovalsPath)
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, `<td>github</td><td>delete_repository</td><td data-label="Session"><a href="/ui/sessions/`, "server, tool and a link to the session")
	assert.Contains(t, page, `github.com/acme/repo</a>`, "the session is shown by its workspace")
	assert.NotContains(t, page, ">session-"+approvalID(0)+"<", "the full session ID is not shown as text")
	assert.Contains(t, page, ui.ApprovalPath(approvalID(0)))
	assert.NotContains(t, page, approvalID(1), "an expired approval isn't listed")
	assert.Contains(t, page, `hx-get="/ui/approvals" hx-trigger="every 5s"`, "htmx refreshes the list every 5 s")
}

func TestApprovalDetailSaysPayloadsAreOff(t *testing.T) {
	env := startUIEnv(t, "")
	id := approvalID(0)
	env.insertApproval(t, id, "dave", approvalTimeout)
	browser := env.insertSession(t, "detail-session", nil)

	response := get(t, browser, env.server.URL+ui.ApprovalPath(id))
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, "Arguments aren&#39;t shown because audit payloads are off.")
	assert.Contains(t, page, "session-"+id)
	assert.Contains(t, page, "github.com/acme/repo")
}

func TestApprovalLinkWithoutSessionGoesToStepUp(t *testing.T) {
	env := startUIEnv(t, "")
	id := approvalID(0)
	browser := env.browser(t)

	response := get(t, browser, env.server.URL+ui.ApprovalPath(id))
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.StepUpURL(ui.ApprovalPath(id)), response.Header.Get("Location"))
}

func TestApproveNeedsAFreshStepUp(t *testing.T) {
	env := startUIEnv(t, "")
	id := approvalID(0)
	env.insertApproval(t, id, "dave", approvalTimeout)
	browser := env.insertSession(t, "stale-session", nil)
	env.ageSignIn(t, "stale-session", 5*time.Minute+time.Second)

	page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
	assert.Contains(t, page, `href="`+html.EscapeString(ui.StepUpURL(ui.ApprovalPath(id)))+`">Approve</a>`, "Approve goes through the step-up")
	assert.Contains(t, page, "You'll be asked to sign in again.")
	assert.NotContains(t, page, "Approving needs a fresh sign-in.")
	assert.Contains(t, page, `<a href="/ui/approvals">All approvals</a>`)
	assert.Contains(t, page, "Made by")
	assert.NotContains(t, page, "/approve", "no approve form without a fresh sign-in")
	assert.Contains(t, page, "/deny", "deny needs no step-up")

	response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("stale-session")}})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.StepUpURL(ui.ApprovalPath(id)), response.Header.Get("Location"))
	assert.Equal(t, "pending", env.approvalRow(t, id).Status)
	assert.Empty(t, env.approvalEvents(t, "approval.approve", id))
}

func TestApproveAndDeny(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "decide-session", nil)
	env.ageSignIn(t, "decide-session", time.Minute)

	token := url.Values{ui.CSRFField: {csrfTokenOf("decide-session")}}
	next := env.listen(t)

	outcome := func(outcome string) float64 {
		return opsmetrics.Value("toolgate_approval_requests_total", map[string]string{"kind": "tool", "outcome": outcome})
	}
	approved, denied := outcome("approved"), outcome("denied")

	t.Run("approve", func(t *testing.T) {
		id := approvalID(0)
		env.insertApproval(t, id, "dave", time.Minute)

		page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "/approve", "a fresh session sees the approve form")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", token)
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, "approval:"+id, next())

		row := env.approvalRow(t, id)
		assert.Equal(t, "approved", row.Status)
		assert.InDelta(t, approved+1, outcome("approved"), 0)
		assert.InDelta(t, denied, outcome("denied"), 0)
		assert.Equal(t, "dave", row.DecidedBy.String)
		assert.WithinDuration(t, time.Now().Add(approvalTimeout), row.ExpiresAt.Time, time.Minute,
			"expires_at is now() + approvalTimeout")

		assert.Equal(t, []map[string]string{{
			"principal": "dave", "workspace": "", "session": "", "attestation": "user", "server": "github", "tool": "delete_repository",
		}}, env.approvalEvents(t, "approval.approve", id))

		page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "This approval was approved.")
	})

	t.Run("deny with a stale sign-in", func(t *testing.T) {
		id := approvalID(1)
		env.insertApproval(t, id, "dave", time.Minute)
		env.ageSignIn(t, "decide-session", time.Hour)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/deny", token)
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, "approval:"+id, next())

		assert.Equal(t, "denied", env.approvalRow(t, id).Status)
		assert.InDelta(t, denied+1, outcome("denied"), 0)
		assert.Len(t, env.approvalEvents(t, "approval.deny", id), 1)
	})
}

func TestDecisionsShowTheRealStatus(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "race-session", nil)
	env.ageSignIn(t, "race-session", 0)

	token := url.Values{ui.CSRFField: {csrfTokenOf("race-session")}}

	t.Run("two tabs", func(t *testing.T) {
		id := approvalID(0)
		env.insertApproval(t, id, "dave", time.Minute)

		require.Equal(t, http.StatusSeeOther, post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/deny", token).StatusCode)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", token)
		assert.Equal(t, http.StatusConflict, response.StatusCode)
		assert.Contains(t, readBody(t, response), "This approval was denied.")
		assert.Equal(t, "denied", env.approvalRow(t, id).Status)
		assert.Empty(t, env.approvalEvents(t, "approval.approve", id))
	})

	for _, action := range []string{"approve", "deny"} {
		t.Run(action+" after expiry", func(t *testing.T) {
			id := approvalID(1)
			if action == "deny" {
				id = approvalID(2)
			}

			env.insertApproval(t, id, "dave", -time.Second)

			response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/"+action, token)
			assert.Equal(t, http.StatusConflict, response.StatusCode)
			assert.Contains(t, readBody(t, response), "This approval expired.")
			assert.Equal(t, "pending", env.approvalRow(t, id).Status)
			assert.Empty(t, env.approvalEvents(t, "approval."+action, id))
		})
	}

	t.Run("expired by the leader's pass", func(t *testing.T) {
		id := approvalID(3)
		env.insertApproval(t, id, "dave", time.Minute)
		_, err := env.pool.Exec(t.Context(), "UPDATE approvals SET status = 'expired' WHERE id = $1", id)
		require.NoError(t, err)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", token)
		assert.Equal(t, http.StatusConflict, response.StatusCode)
		assert.Contains(t, readBody(t, response), "This approval expired.")
	})
}

func TestApprovalsListShowsAccessRequests(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()
	browser := env.insertSession(t, "ar-list-session", nil)

	read := func(t *testing.T) string {
		t.Helper()

		response := get(t, browser, env.server.URL+ui.ApprovalsPath)
		require.Equal(t, http.StatusOK, response.StatusCode)

		return readBody(t, response)
	}

	t.Run("an empty list says nothing is waiting", func(t *testing.T) {
		page := read(t)
		assert.Contains(t, page, "No approvals are waiting for you.")
		assert.NotContains(t, page, "Access changes")
	})

	erin := auth.Identity{Principal: "erin", Groups: []string{"dev"}, Kind: auth.KindUser, Attestation: auth.AttestationUser}
	result, err := env.workspaces.Request(t.Context(), erin, gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Verified: gateway.ScopeAll})
	require.NoError(t, err)
	require.NotEmpty(t, result.Request)

	t.Run("another user's request is not listed", func(t *testing.T) {
		assert.NotContains(t, read(t), result.Request)
	})

	defaultsID := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Verified: gateway.ScopeAll})

	t.Run("a pending access request shows without tool approvals", func(t *testing.T) {
		page := read(t)
		assert.Contains(t, page, "Access changes")
		assert.Contains(t, page, ui.ApprovalPath(defaultsID))
		assert.Contains(t, page, "default for verified sessions: all")
		assert.NotContains(t, page, "No approvals are waiting for you.")
	})

	t.Run("both sections render together", func(t *testing.T) {
		env.insertApproval(t, approvalID(5), "dave", approvalTimeout)

		page := read(t)
		assert.Contains(t, page, ui.ApprovalPath(approvalID(5)))
		assert.Contains(t, page, ui.ApprovalPath(defaultsID))
	})

	t.Run("a denied request moves to the recent list", func(t *testing.T) {
		_, err := db.New(env.pool).DenyAccessRequest(t.Context(), db.DenyAccessRequestParams{ID: defaultsID, Principal: "dave", DecidedBy: pgtype.Text{String: "dave", Valid: true}})
		require.NoError(t, err)

		page := read(t)
		assert.Contains(t, page, "Recent access changes")
		assert.Contains(t, page, "recent-access-request-"+defaultsID)
		assert.NotContains(t, page, `id="access-request-`+defaultsID)
	})
}

func TestSidebarApprovalCountFollowsDecisions(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "badge-session", nil)
	env.ageSignIn(t, "badge-session", 0)

	token := url.Values{ui.CSRFField: {csrfTokenOf("badge-session")}}

	env.insertApproval(t, approvalID(0), "dave", time.Minute)
	env.insertApproval(t, approvalID(1), "dave", time.Minute)

	page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalsPath))
	assert.Contains(t, page, `title="2 waiting for you"`)

	response := post(t, browser, env.server.URL+ui.ApprovalPath(approvalID(0))+"/deny", token)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalsPath))
	assert.Contains(t, page, `title="1 waiting for you"`)

	response = post(t, browser, env.server.URL+ui.ApprovalPath(approvalID(1))+"/approve", token)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalsPath))
	assert.NotContains(t, page, "waiting for you\"")
}
