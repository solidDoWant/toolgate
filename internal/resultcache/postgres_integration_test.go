//go:build integration

package resultcache_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/resultcache"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	testEntryLimit    = 1024
	testTemplateLimit = 4096
	testTTL           = time.Hour
)

type fixture struct {
	pool  *pgxpool.Pool
	cache *resultcache.Postgres
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	return fixture{pool: pool, cache: resultcache.NewPostgres(pool, testEntryLimit, testTemplateLimit)}
}

func (f fixture) session(t *testing.T, name string) db.Session {
	t.Helper()

	session, err := db.New(f.pool).UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "alice", SessionID: name, Workspace: "claimed/default", Attestation: "user", Kind: "user",
		Groups: []string{},
	})
	require.NoError(t, err)

	return session
}

func (f fixture) count(t *testing.T) int {
	t.Helper()

	var count int

	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM tool_result_cache`).Scan(&count))

	return count
}

func (f fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), statement, args...)
	require.NoError(t, err)
}

func sharedKey(uid string, generation int64, arguments string) resultcache.Key {
	return resultcache.Key{TemplateUID: uid, Generation: generation, Tool: "search", Scope: "shared", Arguments: arguments}
}

func sessionKey(uid string, session db.Session, arguments string) resultcache.Key {
	return resultcache.Key{
		TemplateUID: uid, Generation: 1, Tool: "search", Scope: "per-session",
		ScopeID: strconv.FormatInt(session.ID, 10), Arguments: arguments,
	}
}

func bytesOf(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = 'x'
	}

	return out
}

func TestPutThenGetReturnsBytesAndDatabaseAge(t *testing.T) {
	f := newFixture(t)
	key := sharedKey("uid-1", 1, `{"q":"a"}`)
	body := []byte(`{"content":[{"type":"text","text":"hello"}]}`)

	_, found, err := f.cache.Get(t.Context(), key)
	require.NoError(t, err)
	assert.False(t, found)

	stored, err := f.cache.Put(t.Context(), key, body, testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored)

	f.exec(t, `UPDATE tool_result_cache SET stored_at = now() - interval '90 seconds'`)

	hit, found, err := f.cache.Get(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, body, hit.Result)
	assert.GreaterOrEqual(t, hit.Age, 90*time.Second)
	assert.Less(t, hit.Age, 5*time.Minute)
}

func TestGetMissesAnExpiredEntry(t *testing.T) {
	f := newFixture(t)
	key := sharedKey("uid-1", 1, `{}`)

	_, err := f.cache.Put(t.Context(), key, []byte(`{}`), testTTL)
	require.NoError(t, err)

	f.exec(t, `UPDATE tool_result_cache SET expires_at = now() - interval '1 second'`)

	_, found, err := f.cache.Get(t.Context(), key)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestGetRechecksEveryKeyColumn(t *testing.T) {
	f := newFixture(t)
	key := sharedKey("uid-1", 1, `{}`)

	_, err := f.cache.Put(t.Context(), key, []byte(`{}`), testTTL)
	require.NoError(t, err)

	// Each case changes one column of the row, so the hash still matches
	// the row's key while the column no longer matches the lookup.
	columns := map[string]string{
		"template_uid": `UPDATE tool_result_cache SET template_uid = 'other'`,
		"generation":   `UPDATE tool_result_cache SET generation = 2`,
		"tool":         `UPDATE tool_result_cache SET tool = 'other'`,
		"scope_id":     `UPDATE tool_result_cache SET scope_id = 'other'`,
	}

	for column, statement := range columns {
		t.Run(column, func(t *testing.T) {
			f.exec(t, `UPDATE tool_result_cache SET template_uid = 'uid-1', generation = 1, tool = 'search', scope_id = ''`)
			_, found, err := f.cache.Get(t.Context(), key)
			require.NoError(t, err)
			require.True(t, found, "the unchanged row is a hit")

			f.exec(t, statement)

			_, found, err = f.cache.Get(t.Context(), key)
			require.NoError(t, err)
			assert.False(t, found)
		})
	}

	t.Run("different key", func(t *testing.T) {
		_, found, err := f.cache.Get(t.Context(), sharedKey("uid-1", 1, `{"q":1}`))
		require.NoError(t, err)
		assert.False(t, found)
	})
}

func TestPutRefusesAnEntryOverTheEntryLimit(t *testing.T) {
	f := newFixture(t)

	stored, err := f.cache.Put(t.Context(), sharedKey("uid-1", 1, `a`), bytesOf(testEntryLimit), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored, "exactly the limit is stored")

	stored, err = f.cache.Put(t.Context(), sharedKey("uid-1", 1, `b`), bytesOf(testEntryLimit+1), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutEntryTooLarge, stored)
	assert.Equal(t, 1, f.count(t))
}

func TestPutRefusesWhenTheTemplateIsFull(t *testing.T) {
	f := newFixture(t)

	for i := range 3 {
		stored, err := f.cache.Put(t.Context(), sharedKey("uid-1", 1, strconv.Itoa(i)), bytesOf(testEntryLimit), testTTL)
		require.NoError(t, err)
		require.Equal(t, resultcache.PutStored, stored)
	}

	// 3072 bytes used. 1024 more fits exactly; 1025 is over the limit.
	stored, err := f.cache.Put(t.Context(), sharedKey("uid-1", 1, "exact"), bytesOf(testEntryLimit), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored, "exactly the template limit is stored")

	stored, err = f.cache.Put(t.Context(), sharedKey("uid-1", 1, "over"), bytesOf(10), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutTemplateFull, stored, "one byte over the limit is refused")
	assert.Equal(t, 4, f.count(t), "nothing is evicted")

	stored, err = f.cache.Put(t.Context(), sharedKey("uid-2", 1, "other"), bytesOf(testEntryLimit), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored, "another template has its own budget")

	stored, err = f.cache.Put(t.Context(), sharedKey("uid-1", 1, "0"), bytesOf(testEntryLimit), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored, "replacing an entry does not count its old size")

	f.exec(t, `UPDATE tool_result_cache SET expires_at = now() - interval '1 second' WHERE tool = 'search' AND template_uid = 'uid-1'`)

	stored, err = f.cache.Put(t.Context(), sharedKey("uid-1", 1, "fresh"), bytesOf(testEntryLimit), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored, "expired entries do not count")
}

func TestPutRefusesAnEndedSession(t *testing.T) {
	f := newFixture(t)
	live := f.session(t, "live")
	ended := f.session(t, "ended")
	require.NoError(t, runtime.EndSession(t.Context(), f.pool, nil, ended.ID, "test"))

	stored, err := f.cache.Put(t.Context(), sessionKey("uid-1", live, "a"), []byte(`{}`), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutStored, stored)

	stored, err = f.cache.Put(t.Context(), sessionKey("uid-1", ended, "a"), []byte(`{}`), testTTL)
	require.NoError(t, err)
	assert.Equal(t, resultcache.PutSessionEnded, stored)
	assert.Equal(t, 1, f.count(t))
}

func TestEndingASessionDeletesOnlyItsEntries(t *testing.T) {
	f := newFixture(t)
	first := f.session(t, "first")
	second := f.session(t, "second")

	for _, key := range []resultcache.Key{
		sessionKey("uid-1", first, "a"), sessionKey("uid-1", second, "a"), sharedKey("uid-1", 1, "a"),
	} {
		stored, err := f.cache.Put(t.Context(), key, []byte(`{}`), testTTL)
		require.NoError(t, err)
		require.Equal(t, resultcache.PutStored, stored)
	}

	require.NoError(t, runtime.EndSession(t.Context(), f.pool, nil, first.ID, "test"))

	var remaining []string

	rows, err := f.pool.Query(t.Context(), `SELECT scope || ':' || scope_id FROM tool_result_cache ORDER BY 1`)
	require.NoError(t, err)

	remaining, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"per-session:" + strconv.FormatInt(second.ID, 10), "shared:"}, remaining)
}

func TestCleanupDeletesStaleEntries(t *testing.T) {
	f := newFixture(t)
	ended := f.session(t, "ended")
	live := f.session(t, "live")

	put := func(key resultcache.Key) {
		stored, err := f.cache.Put(t.Context(), key, []byte(`{}`), testTTL)
		require.NoError(t, err)
		require.Equal(t, resultcache.PutStored, stored)
	}

	put(sharedKey("current", 2, "keep"))
	put(sharedKey("current", 1, "old-generation"))
	put(sharedKey("deleted", 1, "deleted-template"))
	put(sharedKey("current", 2, "expired"))

	liveKey := sessionKey("current", live, "live-session")
	endedKey := sessionKey("current", ended, "ended-session")

	liveKey.Generation, endedKey.Generation = 2, 2

	put(liveKey)
	put(endedKey)

	expiredHash := sharedKey("current", 2, "expired").Hash()
	f.exec(t, `UPDATE tool_result_cache SET expires_at = now() - interval '1 second' WHERE key = $1`, expiredHash[:])
	// An end that skips the delete, as a replica of an older version does.
	f.exec(t, `UPDATE sessions SET ended_at = now() WHERE id = $1`, ended.ID)

	require.NoError(t, f.cache.Cleanup(t.Context(), map[string]int64{"current": 2}))

	var scopes []string

	rows, err := f.pool.Query(t.Context(), `SELECT template_uid || ':' || generation || ':' || scope FROM tool_result_cache ORDER BY 1`)
	require.NoError(t, err)

	scopes, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Len(t, scopes, 2, "one current shared entry and the live session's entry remain")
	assert.Equal(t, []string{"current:2:per-session", "current:2:shared"}, scopes)
}

func TestCleanupDeletesInBatchesAcrossPasses(t *testing.T) {
	const total = 10*1000 + 500

	f := newFixture(t)

	f.exec(t, `INSERT INTO tool_result_cache (key, template_uid, generation, tool, scope, scope_id, result, size_bytes, expires_at)
		SELECT sha256(i::text::bytea), 'uid-1', 1, 'search', 'shared', '', '{}', 2, now() - interval '1 minute'
		FROM generate_series(1, $1) AS i`, total)

	require.NoError(t, f.cache.Cleanup(t.Context(), map[string]int64{"uid-1": 1}))
	assert.Equal(t, 500, f.count(t), "one pass deletes at most 10 batches of 1000")

	require.NoError(t, f.cache.Cleanup(t.Context(), map[string]int64{"uid-1": 1}))
	assert.Equal(t, 0, f.count(t))
}

func TestCleanupDeletesStaleTemplatesInBatches(t *testing.T) {
	f := newFixture(t)

	f.exec(t, `INSERT INTO tool_result_cache (key, template_uid, generation, tool, scope, scope_id, result, size_bytes, expires_at)
		SELECT sha256(i::text::bytea), 'gone', 1, 'search', 'shared', '', '{}', 2, now() + interval '1 hour'
		FROM generate_series(1, 10500) AS i`)

	require.NoError(t, f.cache.Cleanup(t.Context(), map[string]int64{}))
	assert.Equal(t, 500, f.count(t))

	require.NoError(t, f.cache.Cleanup(t.Context(), map[string]int64{}))
	assert.Equal(t, 0, f.count(t))
}

func TestConcurrentMissesKeepOneEntry(t *testing.T) {
	f := newFixture(t)
	key := sharedKey("uid-1", 1, `{"q":"same"}`)

	const writers = 8

	start := make(chan struct{})
	results := make(chan resultcache.PutResult, writers)

	var group sync.WaitGroup

	for i := range writers {
		group.Add(1)

		go func() {
			defer group.Done()

			<-start

			stored, err := f.cache.Put(t.Context(), key, []byte(`{"n":`+strconv.Itoa(i)+`}`), testTTL)
			assert.NoError(t, err)

			results <- stored
		}()
	}

	close(start)
	waitFor(t, &group)
	close(results)

	for stored := range results {
		assert.Equal(t, resultcache.PutStored, stored)
	}

	assert.Equal(t, 1, f.count(t))

	_, found, err := f.cache.Get(t.Context(), key)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestStoreRacingSessionEnd(t *testing.T) {
	f := newFixture(t)
	session := f.session(t, "racing")

	start := make(chan struct{})

	var (
		group  sync.WaitGroup
		stored resultcache.PutResult
	)

	group.Add(2)

	go func() {
		defer group.Done()

		<-start

		var err error

		stored, err = f.cache.Put(t.Context(), sessionKey("uid-1", session, "a"), []byte(`{}`), testTTL)
		assert.NoError(t, err)
	}()

	go func() {
		defer group.Done()

		<-start

		assert.NoError(t, runtime.EndSession(t.Context(), f.pool, nil, session.ID, "test"))
	}()

	close(start)
	waitFor(t, &group)

	assert.Contains(t, []resultcache.PutResult{resultcache.PutStored, resultcache.PutSessionEnded}, stored)
	assert.Equal(t, 0, f.count(t), "an entry is deleted by the end, or never stored")
}

func TestKilledStoreConnectionFreesTheTemplateLock(t *testing.T) {
	f := newFixture(t)

	conn, err := pgx.Connect(t.Context(), f.pool.Config().ConnString())
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	tx, err := conn.Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, db.New(tx).LockToolResultCacheTemplate(t.Context(), "uid-1"))

	var pid int

	require.NoError(t, tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	f.exec(t, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid = $1 AND datname = current_database()`, pid)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stored, err := f.cache.Put(ctx, sharedKey("uid-1", 1, "a"), []byte(`{}`), testTTL)
	require.NoError(t, err, "the dead connection's lock is released")
	assert.Equal(t, resultcache.PutStored, stored)
}

func waitFor(t *testing.T, group *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		group.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("goroutines did not finish")
	}
}
