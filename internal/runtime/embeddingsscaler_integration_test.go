//go:build integration

package runtime_test

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	embeddingsNamespace  = "toolgate"
	embeddingsDeployment = "toolgate-embeddings"
	embeddingsIdle       = 15 * time.Minute
)

// scalerFixture is a database and one embeddings Deployment that several
// scalers (leaders one after the other) share.
type scalerFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	kube    *kubefake.Clientset
	// start is later than the migration's demand time by more than the idle
	// timeout, so the server starts idle.
	start time.Time
}

func newScalerFixture(t *testing.T, replicas int32) *scalerFixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	kube := kubefake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: embeddingsNamespace, Name: embeddingsDeployment},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
	})

	return &scalerFixture{pool: pool, queries: db.New(pool), kube: kube, start: time.Now().Add(2 * embeddingsIdle).Truncate(time.Microsecond)}
}

// leader returns a new scaler, as a newly elected leader has, with its own
// gauge and clock.
func (f *scalerFixture) leader(now *time.Time, fence runtime.Fence) (*runtime.EmbeddingsScaler, *prometheus.GaugeVec) {
	desired := runtime.NewEmbeddingsReplicasDesired()

	return &runtime.EmbeddingsScaler{
		Pool: f.pool,
		Deployment: runtime.KubeEmbeddingsDeployment{
			Client: f.kube, Namespace: embeddingsNamespace, Name: embeddingsDeployment,
		},
		IdleTimeout: embeddingsIdle,
		Fence:       fence,
		Desired:     desired,
		Log:         slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Now:         func() time.Time { return *now },
	}, desired
}

func (f *scalerFixture) replicas(t *testing.T) int32 {
	t.Helper()

	deployment, err := f.kube.AppsV1().Deployments(embeddingsNamespace).Get(t.Context(), embeddingsDeployment, metav1.GetOptions{})
	require.NoError(t, err)

	return *deployment.Spec.Replicas
}

func (f *scalerFixture) setReady(t *testing.T, ready int32) {
	t.Helper()

	deployment, err := f.kube.AppsV1().Deployments(embeddingsNamespace).Get(t.Context(), embeddingsDeployment, metav1.GetOptions{})
	require.NoError(t, err)

	deployment.Status.ReadyReplicas = ready
	_, err = f.kube.AppsV1().Deployments(embeddingsNamespace).UpdateStatus(t.Context(), deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.kube.ClearActions()
}

func (f *scalerFixture) state(t *testing.T) string {
	t.Helper()

	state, err := runtime.EmbeddingsServerState(t.Context(), f.pool)
	require.NoError(t, err)

	return state
}

func (f *scalerFixture) toolSearchSession(t *testing.T, sessionID string) db.Session {
	t.Helper()

	session, err := f.queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "user", SessionID: sessionID, Workspace: "claimed/default",
		Attestation: "user", Kind: "user", Groups: []string{}, ToolSearch: true,
	})
	require.NoError(t, err)

	return session
}

// demandAt records embeddings demand at the given time, as a replica does
// for an embeddings request or a tool search session start.
func (f *scalerFixture) demandAt(t *testing.T, at time.Time) {
	t.Helper()

	(&runtime.EmbeddingsDemand{Pool: f.pool, Now: func() time.Time { return at }}).Record(t.Context())
}

func (f *scalerFixture) scales(t *testing.T) int {
	t.Helper()

	count := 0

	for _, action := range f.kube.Actions() {
		if action.GetVerb() == "patch" {
			count++
		}
	}

	return count
}

func holdingLease() *fakeFence {
	fence := &fakeFence{}
	fence.holds.Store(true)

	return fence
}

func TestEmbeddingsScalerStartsServerOnDemandAndPublishesState(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	scaler, desired := fixture.leader(&now, holdingLease())

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(0), fixture.replicas(t), "nothing needed embeddings")
	assert.Equal(t, runtime.EmbeddingsServerDown, fixture.state(t))
	assert.InDelta(t, 0, promtestutil.ToFloat64(desired.WithLabelValues()), 0)

	fixture.demandAt(t, now)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t))
	assert.Equal(t, runtime.EmbeddingsServerStarting, fixture.state(t))
	assert.InDelta(t, 1, promtestutil.ToFloat64(desired.WithLabelValues()), 0)

	fixture.setReady(t, 1)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, runtime.EmbeddingsServerReady, fixture.state(t))
	assert.Zero(t, fixture.scales(t), "a running server isn't scaled again")
}

func TestEmbeddingsScalerIgnoresSessionsWithoutToolSearch(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	scaler, _ := fixture.leader(&now, holdingLease())

	_, err := fixture.queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "user", SessionID: "plain", Workspace: "claimed/default",
		Attestation: "user", Kind: "user", Groups: []string{}, ToolSearch: false,
	})
	require.NoError(t, err)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(0), fixture.replicas(t))
}

func TestEmbeddingsScalerStartsServerOnRecordedDemand(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	scaler, _ := fixture.leader(&now, holdingLease())

	// Another replica needed embeddings, for a search or the catalog sync.
	demand := &runtime.EmbeddingsDemand{Pool: fixture.pool, Now: func() time.Time { return now }}
	demand.Record(t.Context())

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t))
}

func TestEmbeddingsScalerStopsServerAfterIdleTimeout(t *testing.T) {
	fixture := newScalerFixture(t, 1)
	now := fixture.start
	demand := &runtime.EmbeddingsDemand{Pool: fixture.pool, Now: func() time.Time { return now }}
	demand.Record(t.Context())

	scaler, desired := fixture.leader(&now, holdingLease())

	now = fixture.start.Add(embeddingsIdle - time.Second)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t), "the idle timeout hasn't passed")

	now = fixture.start.Add(embeddingsIdle)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(0), fixture.replicas(t))
	assert.Equal(t, runtime.EmbeddingsServerDown, fixture.state(t))
	assert.InDelta(t, 0, promtestutil.ToFloat64(desired.WithLabelValues()), 0)
}

func TestEmbeddingsScalerStopsServerWhileIdleToolSearchSessionLives(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	scaler, _ := fixture.leader(&now, holdingLease())

	// The session started and recorded demand, then sent no embeddings
	// request; a client never says when it exits, so its row stays live.
	fixture.toolSearchSession(t, "open-but-idle")
	fixture.demandAt(t, now)

	require.NoError(t, scaler.Step(t.Context()))
	require.Equal(t, int32(1), fixture.replicas(t))

	now = fixture.start.Add(embeddingsIdle - time.Second)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t), "the idle timeout hasn't passed")

	now = fixture.start.Add(embeddingsIdle)

	require.NoError(t, scaler.Step(t.Context()))
	assert.Equal(t, int32(0), fixture.replicas(t), "a live session without embeddings requests doesn't hold the server up")
	assert.Equal(t, runtime.EmbeddingsServerDown, fixture.state(t))
}

func TestEmbeddingsScalerLeaderFailoverKeepsIdleTimeout(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	fixture.demandAt(t, now)

	first, _ := fixture.leader(&now, holdingLease())
	require.NoError(t, first.Step(t.Context()))
	require.Equal(t, int32(1), fixture.replicas(t))
	fixture.setReady(t, 1)

	now = fixture.start.Add(time.Minute)
	fixture.demandAt(t, now)

	require.NoError(t, first.Step(t.Context()))

	// The first leader is gone; a new one starts with nothing in memory.
	now = fixture.start.Add(2 * time.Minute)
	second, desired := fixture.leader(&now, holdingLease())
	require.NoError(t, second.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t), "the new leader must not stop the server early")
	assert.Equal(t, runtime.EmbeddingsServerReady, fixture.state(t))
	assert.InDelta(t, 1, promtestutil.ToFloat64(desired.WithLabelValues()), 0)
	assert.Zero(t, fixture.scales(t), "the new leader doesn't flap")

	now = fixture.start.Add(time.Minute + embeddingsIdle - time.Second)

	require.NoError(t, second.Step(t.Context()))
	assert.Equal(t, int32(1), fixture.replicas(t), "the idle timeout counts from the last demand the database holds")

	now = fixture.start.Add(time.Minute + embeddingsIdle)

	require.NoError(t, second.Step(t.Context()))
	assert.Equal(t, int32(0), fixture.replicas(t), "the server must not stay up past the idle timeout")
	assert.Equal(t, 1, fixture.scales(t))
}

func TestEmbeddingsScalerDoesNothingWithoutLease(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	fixture.demandAt(t, now)

	follower, _ := fixture.leader(&now, &fakeFence{})
	require.NoError(t, follower.Step(t.Context()))

	assert.Equal(t, int32(0), fixture.replicas(t))
	assert.Zero(t, fixture.scales(t))

	// Someone else started the server: the leader would publish starting,
	// but the follower publishes nothing.
	_, err := fixture.kube.AppsV1().Deployments(embeddingsNamespace).Patch(t.Context(), embeddingsDeployment,
		types.MergePatchType, []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{}, "scale")
	require.NoError(t, err)
	require.NoError(t, follower.Step(t.Context()))

	server, err := fixture.queries.GetEmbeddingsServer(t.Context())
	require.NoError(t, err)
	assert.Equal(t, runtime.EmbeddingsServerDown, server.State)
}

func TestEmbeddingsScalerOnlyGetsDeploymentAndPatchesScale(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	scaler, _ := fixture.leader(&now, holdingLease())
	fixture.demandAt(t, now)

	require.NoError(t, scaler.Step(t.Context()))

	now = fixture.start.Add(embeddingsIdle)

	require.NoError(t, scaler.Step(t.Context()))
	require.Equal(t, int32(0), fixture.replicas(t))

	for _, action := range fixture.kube.Actions() {
		assert.Equal(t, embeddingsNamespace, action.GetNamespace())
		assert.Equal(t, "deployments", action.GetResource().Resource)

		switch action.GetVerb() {
		case "get":
			assert.Empty(t, action.GetSubresource())
		case "patch":
			assert.Equal(t, "scale", action.GetSubresource())
		default:
			t.Errorf("unexpected %s on deployments", action.GetVerb())
		}
	}
}

func TestEmbeddingsDemandWritesAtMostOncePerInterval(t *testing.T) {
	fixture := newScalerFixture(t, 0)
	now := fixture.start
	demand := &runtime.EmbeddingsDemand{Pool: fixture.pool, Interval: 5 * time.Second, Now: func() time.Time { return now }}

	lastDemand := func() time.Time {
		server, err := fixture.queries.GetEmbeddingsServer(t.Context())
		require.NoError(t, err)

		return server.LastDemandAt.Time
	}

	demand.Record(t.Context())
	assert.True(t, lastDemand().Equal(fixture.start))

	now = fixture.start.Add(4 * time.Second)

	demand.Record(t.Context())
	assert.True(t, lastDemand().Equal(fixture.start), "a second demand within the interval isn't written")

	now = fixture.start.Add(5 * time.Second)

	demand.Record(t.Context())
	assert.True(t, lastDemand().Equal(now))

	// An older demand never moves the time back.
	require.NoError(t, fixture.queries.RecordEmbeddingsDemand(t.Context(), pgtype.Timestamptz{Time: fixture.start, Valid: true}))
	assert.True(t, lastDemand().Equal(now))
}
