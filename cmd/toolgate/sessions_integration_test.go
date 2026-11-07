//go:build integration

package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestSessionsCommand(t *testing.T) {
	isolateUserConfig(t)

	served := startServeWithSecrets(t)
	defer stopServe(t, served.stderr, served.done)

	served.provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}})

	code, _, signInStderr := signInApproving(t, served.provider, "alice", "--gateway", served.url)
	require.Equal(t, 0, code, "stderr: %s", signInStderr)

	t.Run("toolgate sessions lists the user's open sessions and their enabled servers", func(t *testing.T) {
		_ = served.connectAs(t, "session-one", "laptop-widgets")
		_ = served.connectAs(t, "session-two", "")

		conn, err := pgx.Connect(t.Context(), served.databaseURL)
		require.NoError(t, err)

		defer func() { _ = conn.Close(t.Context()) }()

		for _, name := range []string{"github", "files"} {
			_, err = conn.Exec(t.Context(), `INSERT INTO session_servers (session_pk, template_ns, template_name)
				SELECT id, 'catalog', $1 FROM sessions WHERE session_id = 'session-one'`, name)
			require.NoError(t, err)
		}

		// Another user's session and an ended one are not listed.
		_, err = conn.Exec(t.Context(), `INSERT INTO sessions (principal, session_id, workspace, attestation) VALUES ('mallory', 'session-other', 'w', 'user')`)
		require.NoError(t, err)
		_, err = conn.Exec(t.Context(), `INSERT INTO sessions (principal, session_id, workspace, attestation, ended_at) VALUES ('alice', 'session-ended', 'w', 'user', now())`)
		require.NoError(t, err)

		code, out, errOut := runCLI(t, "", "sessions")
		require.Equal(t, 0, code, "stderr: %s", errOut)

		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 3, "a header and two sessions: %q", out)
		assert.Regexp(t, `^SESSION\s+WORKSPACE\s+LAST SEEN\s+SERVERS$`, lines[0])

		rows := strings.Join(lines[1:], "\n")
		assert.Regexp(t, `(?m)^session-one\s+claimed/laptop-widgets\s+\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\s+files,github$`, rows)
		assert.Regexp(t, `(?m)^session-two\s+claimed/default\s+\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\s+-$`, rows)
		assert.NotContains(t, out, "session-other")
		assert.NotContains(t, out, "session-ended")
	})
}
