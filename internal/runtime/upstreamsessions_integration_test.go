//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// sweepFixture is a session that has the template enabled, and a recording
// upstream the template points at. The template is a remote one so the test
// server can be reached; a test that wants an in-cluster instance gives it a
// namespace, and one that wants a remote instance leaves the namespace empty.
type sweepFixture struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	sweeper  *runtime.UpstreamSessionSweeper
	upstream *recordingUpstream
	session  db.Session
	// remoteAuth is what the sweeper's RemoteAuth returns.
	remoteAuth func() (*runtime.RemoteAuth, error)
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()

	queries, startStore := catalogPool(t)
	upstream := newRecordingUpstream(t)
	template := remoteTemplate(upstream.url)

	session, err := queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "alice", SessionID: "sweep", Workspace: "claimed/default", Attestation: "user", Kind: "user",
		Groups: []string{},
	})
	require.NoError(t, err)

	_, err = startStore.Pool.Exec(t.Context(),
		"INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, $2, $3)",
		session.ID, template.Namespace, template.Name)
	require.NoError(t, err)

	fixture := &sweepFixture{
		pool: startStore.Pool, queries: queries, upstream: upstream, session: session,
		remoteAuth: func() (*runtime.RemoteAuth, error) {
			return &runtime.RemoteAuth{Header: http.Header{"Authorization": {"Bearer owner-key"}}}, nil
		},
	}
	fixture.sweeper = &runtime.UpstreamSessionSweeper{
		Pool: startStore.Pool,
		Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			return template.DeepCopy(), true
		},
		RemoteAuth: func(_ context.Context, _ db.Instance, _ *v1alpha1.MCPServerTemplate) (*runtime.RemoteAuth, error) {
			return fixture.remoteAuth()
		},
	}

	return fixture
}

// instance inserts an instance of the template and returns its ID.
func (f *sweepFixture) instance(t *testing.T, id, namespace string) {
	t.Helper()

	_, err := f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID, Generation: 3,
		Scope: "per-user", ScopeKey: "user:" + id, OwnerPrincipal: "alice", Namespace: namespace,
	})
	require.NoError(t, err)
}

// store opens a real upstream session and stores a live row for it.
func (f *sweepFixture) store(t *testing.T, instanceID string) string {
	t.Helper()

	opened, _ := forwardingCopy(t, f.upstream, nil)
	t.Cleanup(func() { _ = opened.Close() })

	result, err := json.Marshal(opened.InitializeResult())
	require.NoError(t, err)

	_, err = f.queries.InsertUpstreamSession(t.Context(), db.InsertUpstreamSessionParams{
		InstanceID: instanceID, SessionPk: f.session.ID, UpstreamSessionID: opened.ID(), InitializeResult: result,
	})
	require.NoError(t, err)

	return opened.ID()
}

func (f *sweepFixture) rows(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM upstream_sessions").Scan(&count))

	return count
}

func (f *sweepFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), sql, args...)
	require.NoError(t, err)
}

func TestInstanceStopRemovesRowsWithoutDelete(t *testing.T) {
	tests := []struct {
		name string
		stop string
	}{
		{name: "stopped", stop: "UPDATE instances SET status = 'stopped'"},
		{name: "stopping while replaced", stop: "UPDATE instances SET status = 'stopping'"},
		{name: "failed", stop: "UPDATE instances SET desired = 'stopped', status = 'failed'"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.instance(t, instanceID, "tg-one")
			f.store(t, instanceID)
			f.upstream.reset()

			f.exec(t, test.stop)
			require.NoError(t, f.sweeper.Sweep(t.Context()))

			assert.Zero(t, f.rows(t))
			assert.Zero(t, f.upstream.count(http.MethodDelete, ""))
		})
	}
}

func TestSweepEndsRemoteUpstreamSessionAfterStop(t *testing.T) {
	f := newSweepFixture(t)
	f.instance(t, instanceID, "")
	id := f.store(t, instanceID)
	f.upstream.reset()

	f.exec(t, "UPDATE instances SET status = 'stopped'")
	require.NoError(t, f.sweeper.Sweep(t.Context()))

	assert.Zero(t, f.rows(t))

	var deletes []seenRequest

	for _, seen := range f.upstream.requests() {
		if seen.httpMethod == http.MethodDelete {
			deletes = append(deletes, seen)
		}
	}

	require.Len(t, deletes, 1)
	assert.Equal(t, id, deletes[0].sessionID)
	assert.Equal(t, "Bearer owner-key", deletes[0].authorization)
}

func TestSweepSendsNothingWhenRemoteHeadersCannotBeFilled(t *testing.T) {
	f := newSweepFixture(t)
	f.instance(t, instanceID, "")
	f.store(t, instanceID)
	f.upstream.reset()

	f.remoteAuth = func() (*runtime.RemoteAuth, error) { return nil, errors.New("secret deleted") }

	f.exec(t, "UPDATE instances SET status = 'stopped'")
	require.NoError(t, f.sweeper.Sweep(t.Context()))

	assert.Zero(t, f.rows(t))
	assert.Empty(t, f.upstream.requests())
}

func TestSweepDeletesUpstreamSessionOfLiveInstanceOnce(t *testing.T) {
	tests := []struct {
		name string
		end  string
	}{
		{name: "session ended", end: "UPDATE sessions SET ended_at = now()"},
		{name: "server disabled", end: "DELETE FROM session_servers"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.instance(t, instanceID, "tg-one")
			id := f.store(t, instanceID)
			f.upstream.reset()

			f.exec(t, test.end)
			require.NoError(t, f.sweeper.Sweep(t.Context()))
			require.NoError(t, f.sweeper.Sweep(t.Context()))

			assert.Zero(t, f.rows(t))
			assert.Equal(t, 1, f.upstream.count(http.MethodDelete, ""))

			for _, seen := range f.upstream.requests() {
				assert.Equal(t, id, seen.sessionID)
			}
		})
	}
}

func TestSweepKeepsRowsOfValidPairs(t *testing.T) {
	f := newSweepFixture(t)
	f.instance(t, instanceID, "tg-one")
	f.store(t, instanceID)
	f.upstream.reset()

	require.NoError(t, f.sweeper.Sweep(t.Context()))

	assert.Equal(t, 1, f.rows(t))
	assert.Empty(t, f.upstream.requests())
}

func TestSweepAfterLeaderChangeIsSafeToRepeat(t *testing.T) {
	f := newSweepFixture(t)
	f.instance(t, instanceID, "tg-one")
	id := f.store(t, instanceID)

	var row db.GetLiveUpstreamSessionRow

	row, err := f.queries.GetLiveUpstreamSession(t.Context(), db.GetLiveUpstreamSessionParams{
		InstanceID: instanceID, SessionPk: f.session.ID,
	})
	require.NoError(t, err)

	f.exec(t, "UPDATE sessions SET ended_at = now()")
	require.NoError(t, f.sweeper.Sweep(t.Context()))
	require.Zero(t, f.rows(t))
	require.Equal(t, 1, f.upstream.count(http.MethodDelete, ""))

	// A former leader that read the row before the first run still sends
	// its DELETE, and the server no longer knows the session.
	_, err = f.pool.Exec(t.Context(),
		"INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result, ended_at) VALUES ($1, $2, $3, $4, now())",
		instanceID, f.session.ID, id, row.InitializeResult)
	require.NoError(t, err)

	require.NoError(t, f.sweeper.Sweep(t.Context()))
	assert.Zero(t, f.rows(t))
	assert.Equal(t, 2, f.upstream.count(http.MethodDelete, ""))

	// A run that finds nothing to do matches no rows.
	require.NoError(t, f.sweeper.Sweep(t.Context()))
}

func TestSweepRunsOnlyOnTheLeaseHolder(t *testing.T) {
	sweeper := &runtime.UpstreamSessionSweeper{}

	assert.True(t, sweeper.NeedLeaderElection())
}
