//go:build integration

package store_test

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func insertTestInstance(t *testing.T, queries *db.Queries, id string) {
	t.Helper()

	_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: "catalog", TemplateName: "files", TemplateUid: "uid-1", Generation: 1,
		Scope: "per-user", ScopeKey: "user:alice:" + id, OwnerPrincipal: "alice", Namespace: "tg-" + id,
	})
	require.NoError(t, err)
}

func TestMigrateKeepsStoppingInstancesWithoutStoppingTime(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	sqlDB := stdlib.OpenDBFromPool(pool)

	t.Cleanup(func() { _ = sqlDB.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 14)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO instances (id, template_ns, template_name, template_uid, generation, scope, scope_key,
		owner_principal, namespace, desired, status)
		VALUES ($1, 'catalog', 'files', 'uid-1', 1, 'per-user', 'user:alice', 'alice', 'tg-x', 'stopped', 'stopping')`, rowID(1))
	require.NoError(t, err)

	require.NoError(t, store.Migrate(ctx, pool))

	var stoppingAt *string

	require.NoError(t, pool.QueryRow(ctx, "SELECT stopping_at::text FROM instances WHERE id = $1", rowID(1)).Scan(&stoppingAt))
	assert.Nil(t, stoppingAt)
}

func TestPersistentVolumeChecksRefuseBadRows(t *testing.T) {
	_, pool := migratedPool(t)

	const insert = `INSERT INTO persistent_volumes (id, template_ns, template_name, template_uid, volume_name, scope, scope_id, storage_class, size_bytes, delete_reason)
		VALUES ($1, 'catalog', 'files', 'uid-1', $2, $3, $4, 'fast', $5, $6)`

	tests := []struct {
		name                       string
		id, volume, scope, scopeID string
		size                       int64
		reason                     string
		wantErr                    require.ErrorAssertionFunc
	}{
		{name: "valid per-template", id: rowID(1), volume: "a", scope: "per-template", size: 1, wantErr: require.NoError},
		{name: "valid per-user", id: rowID(2), volume: "a", scope: "per-user", scopeID: "alice", size: 1, wantErr: require.NoError},
		{name: "bad id", id: "XYZ", volume: "b", scope: "per-template", size: 1, wantErr: require.Error},
		{name: "per-template with scope ID", id: rowID(3), volume: "c", scope: "per-template", scopeID: "alice", size: 1, wantErr: require.Error},
		{name: "per-user without scope ID", id: rowID(4), volume: "d", scope: "per-user", size: 1, wantErr: require.Error},
		{name: "unknown scope", id: rowID(5), volume: "e", scope: "shared", size: 1, wantErr: require.Error},
		{name: "size zero", id: rowID(6), volume: "f", scope: "per-template", size: 0, wantErr: require.Error},
		{name: "unknown delete reason", id: rowID(7), volume: "g", scope: "per-template", size: 1, reason: "bogus", wantErr: require.Error},
		{name: "known delete reason", id: rowID(8), volume: "h", scope: "per-template", size: 1, reason: "admin", wantErr: require.NoError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), insert, test.id, test.volume, test.scope, test.scopeID, test.size, test.reason)
			test.wantErr(t, err)
		})
	}

	_, err := pool.Exec(t.Context(), `UPDATE persistent_volumes SET problem = $1 WHERE id = $2`, string(make([]byte, 0)), rowID(1))
	require.NoError(t, err)

	long := make([]byte, store.MaxPersistentVolumeProblemBytes)
	for index := range long {
		long[index] = 'a'
	}

	_, err = pool.Exec(t.Context(), `UPDATE persistent_volumes SET problem = $1 WHERE id = $2`, string(long), rowID(1))
	require.NoError(t, err, "exactly the limit")

	_, err = pool.Exec(t.Context(), `UPDATE persistent_volumes SET problem = $1 WHERE id = $2`, string(long)+"a", rowID(1))
	require.Error(t, err, "one byte over the limit")
}

func TestStoppedWriteReleasesHeldVolumes(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)

	insertTestInstance(t, queries, rowID(10))
	insertTestInstance(t, queries, rowID(11))

	_, err := pool.Exec(ctx, `INSERT INTO persistent_volumes (id, template_ns, template_name, template_uid, volume_name, scope, scope_id, storage_class, size_bytes, instance_id, last_used_at)
		VALUES ($1, 'catalog', 'files', 'uid-1', 'a', 'per-template', '', 'fast', 1, $2, now() - interval '1 day'),
		       ($3, 'catalog', 'files', 'uid-1', 'b', 'per-template', '', 'fast', 1, $4, now() - interval '1 day')`,
		rowID(1), rowID(10), rowID(2), rowID(11))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE instances SET desired = 'stopped' WHERE id IN ($1, $2)", rowID(10), rowID(11))
	require.NoError(t, err)

	for _, id := range []string{rowID(10), rowID(11)} {
		changed, err := queries.SetInstanceStopping(ctx, db.SetInstanceStoppingParams{ID: id, FromStatus: "pending"})
		require.NoError(t, err)
		require.EqualValues(t, 1, changed)
	}

	var stoppingAt *string

	require.NoError(t, pool.QueryRow(ctx, "SELECT stopping_at::text FROM instances WHERE id = $1", rowID(10)).Scan(&stoppingAt))
	assert.NotNil(t, stoppingAt)

	changed, err := queries.SetInstanceStopped(ctx, rowID(10))
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)

	var (
		holder *string
		fresh  bool
	)

	require.NoError(t, pool.QueryRow(ctx, "SELECT instance_id, last_used_at > now() - interval '1 hour' FROM persistent_volumes WHERE id = $1", rowID(1)).Scan(&holder, &fresh))
	assert.Nil(t, holder)
	assert.True(t, fresh)

	require.NoError(t, pool.QueryRow(ctx, "SELECT instance_id, last_used_at > now() - interval '1 hour' FROM persistent_volumes WHERE id = $1", rowID(2)).Scan(&holder, &fresh))
	require.NotNil(t, holder, "another instance's volume stays held")
	assert.Equal(t, rowID(11), *holder)
	assert.False(t, fresh)

	changed, err = queries.SetInstanceStopped(ctx, rowID(10))
	require.NoError(t, err)
	assert.Zero(t, changed, "a repeated write changes nothing")
}
