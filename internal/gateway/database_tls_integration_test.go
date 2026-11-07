//go:build integration

package gateway_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// secretVolume mimics how the kubelet updates a mounted Secret: the files are
// symlinks through "..data", which points at a versioned directory, and an
// update swaps "..data" atomically to a new directory.
type secretVolume struct {
	dir      string
	versions int
}

func newSecretVolume(t *testing.T, certFile, keyFile string) *secretVolume {
	t.Helper()

	volume := &secretVolume{dir: t.TempDir()}
	volume.update(t, certFile, keyFile)

	for _, name := range []string{"tls.crt", "tls.key"} {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(volume.dir, name)))
	}

	return volume
}

func (v *secretVolume) update(t *testing.T, certFile, keyFile string) {
	t.Helper()

	v.versions++
	version := filepath.Join(v.dir, "..version"+strconv.Itoa(v.versions))
	require.NoError(t, os.Mkdir(version, 0o700))

	for name, from := range map[string]string{"tls.crt": certFile, "tls.key": keyFile} {
		content, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(version, name), content, 0o600))
	}

	staging := filepath.Join(v.dir, "..data_tmp")
	require.NoError(t, os.Symlink(filepath.Base(version), staging))
	require.NoError(t, os.Rename(staging, filepath.Join(v.dir, "..data")))
}

func (v *secretVolume) certFile() string { return filepath.Join(v.dir, "tls.crt") }

func (v *secretVolume) keyFile() string { return filepath.Join(v.dir, "tls.key") }

func TestGatewayUsesRotatedDatabaseClientCertificate(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := t.Context()

	admin, err := pgx.Connect(ctx, pg.ConnString())
	require.NoError(t, err)

	t.Cleanup(func() { _ = admin.Close(context.WithoutCancel(ctx)) })

	role := pg.RoleName("gateway")
	_, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
	require.NoError(t, err)

	// The first certificate expires soon after the gateway starts, as a
	// short-lived cert-manager certificate does. The server refuses it from
	// then on, so only the rotated files can open new connections.
	expiry := time.Now().Add(15 * time.Second)
	oldCert, oldKey := pg.IssueClientCertUntil(t, role, expiry)
	volume := newSecretVolume(t, oldCert, oldKey)

	cfg := testConfig()
	cfg.DatabaseURL = pg.ConnStringFor(role, volume.certFile(), volume.keyFile())

	var server *gateway.Server

	running := start(t, cfg, func(s *gateway.Server) { server = s })
	database := server.Database()

	status, body := get(t, running.url("/readyz"), nil)
	require.Equal(t, http.StatusOK, status, body)

	listener := store.NewListener(database, slog.New(slog.DiscardHandler))
	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	listenCtx, stopListener := context.WithCancel(ctx)
	listenDone := make(chan error, 1)

	go func() { listenDone <- listener.Run(listenCtx) }()

	t.Cleanup(func() {
		stopListener()
		assert.ErrorIs(t, <-listenDone, context.Canceled)
	})

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	newCert, newKey := pg.IssueClientCert(t, role)
	volume.update(t, newCert, newKey)

	select {
	case <-time.After(time.Until(expiry) + time.Second):
	case <-ctx.Done():
		t.Fatal("test ended before the first certificate expired")
	}

	// Close every connection the gateway holds, as the pool's connection
	// lifetime does, so the next queries and the listener reconnect.
	var terminated int
	require.NoError(t, admin.QueryRow(ctx,
		"SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1) AS t", role).Scan(&terminated))
	require.Positive(t, terminated)
	database.Reset()

	status, body = get(t, running.url("/readyz"), nil)
	assert.Equal(t, http.StatusOK, status, body)

	var user string
	require.NoError(t, database.QueryRow(ctx, "SELECT current_user").Scan(&user))
	assert.Equal(t, role, user)

	assert.Equal(t, store.Event{Missed: true}, nextNotification(t, events), "the listener reconnects")
	require.NoError(t, store.Notify(ctx, database, store.KindSession, "rotated"))
	assert.Equal(t, store.Event{Kind: store.KindSession, ID: "rotated"}, nextNotification(t, events))
}

func nextNotification(t *testing.T, events <-chan store.Event) store.Event {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(30 * time.Second):
		t.Fatal("no notification within 30s")

		return store.Event{}
	}
}
