//go:build integration

package ui_test

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// insertListedSession stores a session with the fields the list shows.
func (e *uiEnv) insertListedSession(t *testing.T, principal, sessionID, workspace, attestation, client string, seenMinutesAgo, endedMinutesAgo int, toolSearch bool) int64 {
	t.Helper()

	var id int64

	err := e.pool.QueryRow(t.Context(),
		`INSERT INTO sessions (principal, session_id, workspace, attestation, tool_search, last_seen_at, ended_at)
		 VALUES ($1, $2, $3, $4, $5, now() - make_interval(mins => $6),
		         CASE WHEN $7 >= 0 THEN now() - make_interval(mins => $7) END) RETURNING id`,
		principal, sessionID, workspace, attestation, toolSearch, seenMinutesAgo, endedMinutesAgo).Scan(&id)
	require.NoError(t, err)

	if client != "" {
		name, version, _ := strings.Cut(client, " ")
		_, err = e.pool.Exec(t.Context(),
			`INSERT INTO mcp_sessions (id, session_pk, principal, session_id, workspace, attestation, protocol_version, initialize_params)
			 VALUES ($1, $2, $3, $4, $5, $6, '2025-11-25', jsonb_build_object('clientInfo', jsonb_build_object('name', $7::text, 'version', $8::text)))`,
			"mcp-"+sessionID, id, principal, sessionID, workspace, attestation, name, version)
		require.NoError(t, err)
	}

	return id
}

var sessionRowPattern = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)

// rowFor returns the table row that links to session id.
func rowFor(page, link string) string {
	for _, match := range sessionRowPattern.FindAllStringSubmatch(page, -1) {
		if strings.Contains(match[1], link) {
			return match[1]
		}
	}

	return ""
}

func TestSessionsListShowsEachSessionState(t *testing.T) {
	env := startUIEnv(t, "admins")
	user := env.insertSession(t, "list-user", nil)

	active := env.insertListedSession(t, "dave", "7f3a9c1e-1111-2222-3333-444444444444", "coder/main/toolgate-auto", "attested", "claude-code 2.1.319", 0, -1, true)
	idle := env.insertListedSession(t, "dave", "idle-session-0001", "claimed/laptop", "user", "", 30, -1, false)
	_ = env.insertListedSession(t, "dave", "resumable-0001", "ws", "user", "", 120, 60, false)
	_ = env.insertListedSession(t, "dave", "ended-0001", "ws", "user", "", 3000, 2000, false)
	_ = env.insertListedSession(t, "dave", "mcp.abcdef123456", "ws", "user", "", 120, 60, false)

	list := func(query string) string {
		response := get(t, user, env.server.URL+"/ui/sessions"+query)
		require.Equal(t, http.StatusOK, response.StatusCode)

		return readBody(t, response)
	}

	link := func(id int64) string { return "/ui/sessions/" + itoa(id) + `"` }

	t.Run("an active verified helper session", func(t *testing.T) {
		row := rowFor(list("?state=all"), link(active))
		assert.Contains(t, row, "Coder workspace toolgate-auto")
		assert.Contains(t, row, `title="coder/main/toolgate-auto"`)
		assert.Contains(t, row, "verified</span>")
		assert.NotContains(t, row, "unverified")
		assert.Contains(t, row, "claude-code 2.1.319")
		assert.Contains(t, row, "helper</span>")
		assert.Contains(t, row, "badge-ok\">active</span>")
		assert.Contains(t, row, "<td>on</td>")
		assert.Contains(t, row, "just now")
		assert.Contains(t, row, ">7f3a9c1e…</span>")
		assert.Contains(t, row, `value="7f3a9c1e-1111-2222-3333-444444444444"`)
		assert.Contains(t, row, ">you</span>")
	})

	t.Run("an idle unverified session", func(t *testing.T) {
		row := rowFor(list("?state=all"), link(idle))
		assert.Contains(t, row, "unverified: laptop")
		assert.Contains(t, row, "unverified</span>")
		assert.Contains(t, row, ">idle</span>")
		assert.Contains(t, row, "<td>off</td>")
		assert.Contains(t, row, "unknown client")
		assert.Contains(t, row, "30 min ago")
	})

	t.Run("an ended helper session inside the resume window can be resumed", func(t *testing.T) {
		row := rowFor(list("?state=all"), `value="resumable-0001"`)
		assert.Contains(t, row, "resumable until 20")
		assert.NotContains(t, row, "<a href=")
	})

	t.Run("a connector session is never resumable", func(t *testing.T) {
		row := rowFor(list("?state=all"), `value="mcp.abcdef123456"`)
		assert.Contains(t, row, "connector</span>")
		assert.Contains(t, row, ">ended</span>")
		assert.NotContains(t, row, "resumable")
	})

	t.Run("a session ended beyond the resume window is not listed", func(t *testing.T) {
		assert.NotContains(t, list("?state=all"), "ended-0001")
	})

	t.Run("the default is the active sessions of the user", func(t *testing.T) {
		page := list("")
		assert.Contains(t, page, link(active))
		assert.NotContains(t, page, link(idle))
		assert.NotContains(t, page, "resumable")
	})

	t.Run("recent keeps sessions seen in the last 24 hours", func(t *testing.T) {
		page := list("?state=recent")
		assert.Contains(t, page, link(active))
		assert.Contains(t, page, link(idle))
	})

	t.Run("the workspace filter keeps one workspace and its links stay in the URL", func(t *testing.T) {
		page := list("?state=all&workspace=claimed/laptop")
		assert.Contains(t, page, link(idle))
		assert.NotContains(t, page, link(active))
		assert.Contains(t, page, `href="/ui/sessions?state=active&amp;workspace=claimed%2Flaptop"`)
	})

	t.Run("the empty state links to quick setup", func(t *testing.T) {
		page := list("?workspace=none")
		assert.Contains(t, page, "Sessions appear when Claude Code connects through the toolgate plugin")
		assert.Contains(t, page, `href="/ui/setup"`)
	})
}

func TestSessionsListHidesOtherUsersFromNonAdmins(t *testing.T) {
	env := startUIEnv(t, "admins")
	user := env.insertSession(t, "scope-user", nil)
	admin := env.insertSession(t, "scope-admin", []string{"admins"})

	mine := env.insertListedSession(t, "dave", "mine-session", "ws", "user", "", 1, -1, false)
	hashed := "914009fed7908eb8fcb6d9435a3aa8d878cf45ec4470a94c688a25539b0125fb"
	theirs := env.insertListedSession(t, hashed, "theirs-session", "ws", "user", "", 1, -1, false)
	require.NoError(t, db.New(env.pool).UpsertPrincipalProfile(t.Context(), db.UpsertPrincipalProfileParams{
		Principal: hashed, Email: "fred@example.com", Name: "Fred",
	}))

	read := func(client *http.Client, query string) string {
		response := get(t, client, env.server.URL+"/ui/sessions"+query)
		require.Equal(t, http.StatusOK, response.StatusCode)

		return readBody(t, response)
	}

	for _, query := range []string{"", "?user=all", "?user=" + hashed} {
		page := read(user, query)
		assert.Contains(t, page, "/ui/sessions/"+itoa(mine)+`"`, query)
		assert.NotContains(t, page, "/ui/sessions/"+itoa(theirs)+`"`, query)
		assert.NotContains(t, page, hashed+`"`, query)
	}

	t.Run("an admin sees their own sessions by default", func(t *testing.T) {
		page := read(admin, "")
		assert.Contains(t, page, "/ui/sessions/"+itoa(mine)+`"`)
		assert.NotContains(t, page, "/ui/sessions/"+itoa(theirs)+`"`)
	})

	t.Run("an admin can pick one user by name or everyone", func(t *testing.T) {
		page := read(admin, "?user="+hashed)
		assert.Contains(t, page, "/ui/sessions/"+itoa(theirs)+`"`)
		assert.NotContains(t, page, "/ui/sessions/"+itoa(mine)+`"`)
		assert.Contains(t, page, "Fred &lt;fred@example.com&gt;")
		assert.NotContains(t, page, ">"+hashed+"<")

		page = read(admin, "?user=all")
		assert.Contains(t, page, "/ui/sessions/"+itoa(theirs)+`"`)
		assert.Contains(t, page, "/ui/sessions/"+itoa(mine)+`"`)
	})
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
