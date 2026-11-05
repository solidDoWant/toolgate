//go:build integration

package testutil_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestStartPostgres(t *testing.T) {
	t.Parallel()

	pg := testutil.StartPostgres(t)

	t.Run("client certificate over TLS is accepted", func(t *testing.T) {
		conn, err := pgx.Connect(t.Context(), pg.ConnString())

		require.NoError(t, err)
		defer func() { _ = conn.Close(t.Context()) }()

		var (
			user string
			ssl  bool
		)

		err = conn.QueryRow(t.Context(),
			"SELECT current_user, ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&user, &ssl)
		require.NoError(t, err)
		assert.Equal(t, pg.User, user)
		assert.True(t, ssl)
	})

	t.Run("connection without a client certificate is refused", func(t *testing.T) {
		conn, err := pgx.Connect(t.Context(), pg.ConnStringWithoutClientCert())
		if conn != nil {
			_ = conn.Close(t.Context())
		}

		require.Error(t, err)
	})

	t.Run("plaintext connection is refused", func(t *testing.T) {
		conn, err := pgx.Connect(t.Context(),
			"postgres://"+pg.User+"@"+pg.Host+":"+pg.Port+"/"+pg.Database+"?sslmode=disable")
		if conn != nil {
			_ = conn.Close(t.Context())
		}

		require.Error(t, err)
	})

	t.Run("client certificate for another user is refused", func(t *testing.T) {
		certFile, keyFile := pg.IssueClientCert(t, "someone-else")

		conn, err := pgx.Connect(t.Context(), pg.ConnStringFor(pg.User, certFile, keyFile))
		if conn != nil {
			_ = conn.Close(t.Context())
		}

		require.Error(t, err)
	})
}
