//go:build integration

package ui_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

const additionWorkspace = "coder/heidi/mail"

// openAttestedSession opens principal's session in additionWorkspace and
// returns its key.
func (e *uiEnv) openAttestedSession(t *testing.T, principal, session string) int64 {
	t.Helper()

	row, err := db.New(e.pool).CreateSession(t.Context(), db.CreateSessionParams{
		Principal: principal, SessionID: session, Workspace: additionWorkspace,
		Attestation: auth.AttestationAttested, Kind: auth.KindUser, Groups: []string{},
	})
	require.NoError(t, err)

	return row.ID
}

// insertServerRequest inserts a pending server request, as an enable
// outside the workspace's list does.
func (e *uiEnv) insertServerRequest(t *testing.T, principal string, sessionPK int64, server string) string {
	t.Helper()

	var id string
	require.NoError(t, e.pool.QueryRow(t.Context(), `INSERT INTO access_requests (id, principal, kind, session_pk, workspace, verified, server, expires_at)
		VALUES (md5(random()::text), $1, 'server', $2, $3, true, $4, now() + interval '10 minutes') RETURNING id`,
		principal, sessionPK, additionWorkspace, server).Scan(&id))

	return id
}

func (e *uiEnv) additionsOf(t *testing.T, sessionPK int64) []string {
	t.Helper()

	rows, err := e.pool.Query(t.Context(), `SELECT server FROM session_additions WHERE session_pk = $1 ORDER BY server`, sessionPK)
	require.NoError(t, err)

	servers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)

	return servers
}

func (e *uiEnv) requestStatusOf(t *testing.T, id string) string {
	t.Helper()

	var status string
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT status FROM access_requests WHERE id = $1`, id).Scan(&status))

	return status
}

// [AC-3-19] In a browser, the user allows a server for one session and,
// after a fresh sign-in, always for the workspace. A second session doesn't
// get the session-only addition, and ending the session removes it.
func TestAC3_19SessionAddition(t *testing.T) {
	env := startUIEnv(t, "")

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 3*time.Minute)
	defer cancel()

	user := toolgatetest.OIDCUser{Subject: "heidi", Email: "heidi@example.com", EmailVerified: true, Groups: []string{"dev"}}
	first := env.openAttestedSession(t, user.Subject, "session-1")
	second := env.openAttestedSession(t, user.Subject, "session-2")

	allowSession := chromedp.CSS(`#allow-session`)
	allowWorkspace := chromedp.CSS(`#allow-workspace`)

	t.Run("[AC-3-19] \"Allow for this session\" adds the server to that session only", func(t *testing.T) {
		id := env.insertServerRequest(t, user.Subject, first, "immich")

		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.ApprovalPath(id))))
		require.NoError(t, chromedp.Do(ctx, toolgatetest.SignInToOIDC(user), chromedp.WaitVisible(allowSession)))

		for _, shown := range []string{"immich", "session-1", additionWorkspace, "Always allow in this workspace", "Deny"} {
			assert.True(t, pageShows(ctx, shown), "the page shows %q", shown)
		}

		require.NoError(t, chromedp.Do(ctx, chromedp.Click(allowSession)))

		assert.Eventually(t, func() bool { return env.requestStatusOf(t, id) == "approved" }, time.Minute, 100*time.Millisecond)
		assert.Eventually(t, func() bool { return pageShows(ctx, "This request was approved.") }, time.Minute, 100*time.Millisecond)
		assert.Equal(t, []string{"immich"}, env.additionsOf(t, first))
		assert.Empty(t, env.additionsOf(t, second), "the second session doesn't get it")

		_, listed := env.listOf(t, user.Subject, additionWorkspace)
		assert.False(t, listed, "no workspace list is created")
	})

	t.Run("[AC-3-19] \"Always allow in this workspace\" needs a fresh sign-in and adds the server to the list", func(t *testing.T) {
		id := env.insertServerRequest(t, user.Subject, first, "paperless")

		_, err := env.pool.Exec(t.Context(), "UPDATE ui_sessions SET auth_time = now() - interval '6 minutes' WHERE principal = $1", user.Subject)
		require.NoError(t, err)

		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.ApprovalPath(id))))
		require.Eventually(t, func() bool { return pageShows(ctx, "Sign in again to continue") }, time.Minute, 100*time.Millisecond)
		assert.False(t, pageShows(ctx, "Always allow in this workspace"), "no approval without a fresh sign-in")

		require.NoError(t, chromedp.Do(ctx, chromedp.Click(chromedp.CSS(`a.button`))))
		require.NoError(t, chromedp.Do(ctx, toolgatetest.SignInToOIDC(user), chromedp.WaitVisible(allowWorkspace)))
		require.NoError(t, chromedp.Do(ctx, chromedp.Click(allowWorkspace)))

		assert.Eventually(t, func() bool { return env.requestStatusOf(t, id) == "approved" }, time.Minute, 100*time.Millisecond)

		servers, listed := env.listOf(t, user.Subject, additionWorkspace)
		assert.True(t, listed)
		assert.Equal(t, []string{"paperless"}, servers)
		assert.Equal(t, []string{"immich"}, env.additionsOf(t, first), "the list choice adds no session addition")
	})

	t.Run("[AC-3-19] ending the session removes its session-only addition", func(t *testing.T) {
		_, err := env.pool.Exec(t.Context(), "UPDATE sessions SET last_seen_at = now() - interval '3 hours' WHERE id = $1", first)
		require.NoError(t, err)

		ender := &runtime.SessionEnder{Pool: env.pool, Log: slog.New(slog.DiscardHandler)}
		require.NoError(t, ender.Run(t.Context()))

		assert.Empty(t, env.additionsOf(t, first))

		servers, _ := env.listOf(t, user.Subject, additionWorkspace)
		assert.Equal(t, []string{"paperless"}, servers, "the workspace list stays")
	})
}
