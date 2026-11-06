//go:build integration

package sessionsapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const idleTimeout = 2 * time.Hour

type fixture struct {
	pool     *pgxpool.Pool
	provider *testutil.OIDCProvider
	url      string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audience: provider.Audience})
	require.NoError(t, err)

	mux := http.NewServeMux()
	(&sessionsapi.Handler{Queries: db.New(pool), IdleTimeout: idleTimeout, Log: slog.New(slog.DiscardHandler)}).
		Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return fixture{pool: pool, provider: provider, url: server.URL}
}

// seed inserts a session last seen ago ago and returns its row ID.
func (f fixture) seed(t *testing.T, principal, session string, ago time.Duration, ended bool) int64 {
	t.Helper()

	var id int64

	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO sessions
		(principal, session_id, workspace, attestation, last_seen_at, ended_at)
		VALUES ($1, $2, 'repo-'||$2, 'user', now() - $3::interval, CASE WHEN $4 THEN now() ELSE NULL END) RETURNING id`,
		principal, session, fmt.Sprintf("%d seconds", int(ago.Seconds())), ended).Scan(&id))

	return id
}

func (f fixture) enable(t *testing.T, sessionPK int64, name string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, 'catalog', $2)`, sessionPK, name)
	require.NoError(t, err)
}

func (f fixture) get(t *testing.T, subject string, groups ...string) (int, sessionsapi.List) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+sessionsapi.Path, nil)
	require.NoError(t, err)

	if subject != "" {
		request.Header.Set("Authorization", "Bearer "+f.provider.MintAccessToken(t, testutil.TokenClaims{
			Subject: subject, Email: subject + "@example.com", EmailVerified: true, Groups: groups,
		}))
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	var list sessionsapi.List
	if response.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &list), string(body))
	}

	return response.StatusCode, list
}

func TestSessionsAPI(t *testing.T) {
	f := newFixture(t)

	t.Run("an unauthenticated request is refused", func(t *testing.T) {
		status, _ := f.get(t, "")
		assert.Equal(t, http.StatusUnauthorized, status)
	})

	older := f.seed(t, "alice", "older", time.Minute, false)
	f.seed(t, "alice", "newer", time.Second, false)
	f.enable(t, older, "zeta")
	f.enable(t, older, "alpha")
	f.seed(t, "alice", "ended", time.Second, true)
	f.seed(t, "alice", "just-inside", idleTimeout-time.Minute, false)
	f.seed(t, "alice", "just-outside", idleTimeout+time.Minute, false)
	f.seed(t, "bob", "bobs", time.Second, false)

	t.Run("lists the caller's open sessions, newest first, with sorted servers", func(t *testing.T) {
		status, list := f.get(t, "alice")
		require.Equal(t, http.StatusOK, status)

		var names []string
		for _, session := range list.Sessions {
			names = append(names, session.Session)
		}

		assert.Equal(t, []string{"newer", "older", "just-inside"}, names, "ended and idle sessions and bob's are left out")
		assert.False(t, list.Truncated)
		assert.Equal(t, "repo-newer", list.Sessions[0].Workspace)
		assert.Equal(t, "user", list.Sessions[0].Attestation)
		assert.Empty(t, list.Sessions[0].Servers)
		require.Len(t, list.Sessions[1].Servers, 2)
		assert.Equal(t, "alpha", list.Sessions[1].Servers[0].Name)
		assert.Equal(t, "zeta", list.Sessions[1].Servers[1].Name)
		assert.False(t, list.Sessions[1].Servers[0].EnabledAt.IsZero())
	})

	t.Run("an admin sees only their own sessions", func(t *testing.T) {
		adminSession := f.seed(t, "carol", "carols", time.Second, false)
		f.enable(t, adminSession, "github")

		status, list := f.get(t, "carol", "admins")
		require.Equal(t, http.StatusOK, status)
		require.Len(t, list.Sessions, 1)
		assert.Equal(t, "carols", list.Sessions[0].Session)
	})

	t.Run("another user's sessions never appear", func(t *testing.T) {
		status, list := f.get(t, "bob")
		require.Equal(t, http.StatusOK, status)
		require.Len(t, list.Sessions, 1)
		assert.Equal(t, "bobs", list.Sessions[0].Session)
	})
}

func TestSessionsAPICap(t *testing.T) {
	f := newFixture(t)

	for _, count := range []int{sessionsapi.MaxSessions, sessionsapi.MaxSessions + 1} {
		_, err := f.pool.Exec(t.Context(), `DELETE FROM sessions`)
		require.NoError(t, err)

		_, err = f.pool.Exec(t.Context(), `INSERT INTO sessions (principal, session_id, workspace, attestation)
			SELECT 'dave', 's' || n, 'w', 'user' FROM generate_series(1, $1) n`, count)
		require.NoError(t, err)

		status, list := f.get(t, "dave")
		require.Equal(t, http.StatusOK, status)
		assert.Len(t, list.Sessions, sessionsapi.MaxSessions, "%d sessions", count)
		assert.Equal(t, count > sessionsapi.MaxSessions, list.Truncated, "%d sessions", count)
	}
}
