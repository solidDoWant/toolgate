package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// States of the bundled embeddings server that the leader publishes in the
// database for every replica to read.
const (
	EmbeddingsServerDown     = "down"
	EmbeddingsServerStarting = "starting"
	EmbeddingsServerReady    = "ready"
)

// Reasons logged with a scale action of the embeddings server.
const (
	scaleReasonDemand = "embeddings were needed within the idle timeout"
	scaleReasonIdle   = "tool search was idle for the idle timeout"
)

// DefaultEmbeddingsScaleInterval is how often the leader checks whether the
// embeddings server should run.
const DefaultEmbeddingsScaleInterval = 5 * time.Second

// EmbeddingsDeployment reads and sets the replicas of the embeddings
// server's Deployment.
type EmbeddingsDeployment interface {
	// Replicas returns the Deployment's desired and ready replicas.
	Replicas(ctx context.Context) (desired, ready int32, err error)
	// Scale sets the desired replicas through the scale subresource.
	Scale(ctx context.Context, replicas int32) error
}

// KubeEmbeddingsDeployment is the EmbeddingsDeployment of one Deployment.
// It only gets the Deployment and patches its scale subresource, so the
// gateway needs no other right on it.
type KubeEmbeddingsDeployment struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

// Replicas gets the Deployment.
func (d KubeEmbeddingsDeployment) Replicas(ctx context.Context) (desired, ready int32, err error) {
	deployment, err := d.Client.AppsV1().Deployments(d.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("get deployment %s/%s: %w", d.Namespace, d.Name, err)
	}

	// The API server defaults a missing replicas field to 1.
	desired = 1
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	return desired, deployment.Status.ReadyReplicas, nil
}

// Scale patches the scale subresource.
func (d KubeEmbeddingsDeployment) Scale(ctx context.Context, replicas int32) error {
	type scaleSpec struct {
		Replicas int32 `json:"replicas"`
	}

	patch, err := json.Marshal(struct {
		Spec scaleSpec `json:"spec"`
	}{Spec: scaleSpec{Replicas: replicas}})
	if err != nil {
		return err
	}

	_, err = d.Client.AppsV1().Deployments(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "scale")
	if err != nil {
		return fmt.Errorf("scale deployment %s/%s: %w", d.Namespace, d.Name, err)
	}

	return nil
}

// EmbeddingsScaler runs on the leader. It scales the bundled embeddings
// server to one replica while tool search is in use, and to zero once it
// has been idle for IdleTimeout. Tool search is in use when a replica
// recorded demand less than IdleTimeout ago: an embeddings request, or the
// start or resume of a session with tool search on. A live session alone is
// not demand, because a client never says when it exits and its session
// stays live long after its last request. Every decision comes from the
// database and the Deployment, so a new leader carries on where the old one
// stopped.
type EmbeddingsScaler struct {
	Pool        *pgxpool.Pool
	Deployment  EmbeddingsDeployment
	IdleTimeout time.Duration
	// Interval is how often Run checks. Zero means
	// DefaultEmbeddingsScaleInterval.
	Interval time.Duration
	// Fence is asked before every scale. Nil means always the leader.
	Fence Fence
	// Desired receives the replicas the scaler wants. Nil is fine.
	Desired *prometheus.GaugeVec
	Log     *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// NewEmbeddingsReplicasDesired returns the leader-only gauge of the replicas
// the scaler wants. It has no series until the leader sets it.
func NewEmbeddingsReplicasDesired() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "toolgate_embeddings_server_replicas_desired",
		Help: "Replicas (0 or 1) that the gateway wants for the bundled embeddings server. Set by the replica holding the leader Lease.",
	}, nil)
}

// Run checks every Interval until ctx ends. An error is logged and the next
// check tries again.
func (s *EmbeddingsScaler) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultEmbeddingsScaleInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := s.Step(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("embeddings server scale check failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Step makes one decision: it scales the Deployment between 0 and 1 when
// needed and publishes the server's state. It never changes a Deployment
// that runs more than one replica to anything but zero.
func (s *EmbeddingsScaler) Step(ctx context.Context) error {
	now := s.now()
	queries := db.New(s.Pool)

	server, err := queries.GetEmbeddingsServer(ctx)
	if err != nil {
		return fmt.Errorf("read embeddings server: %w", err)
	}

	desired, ready, err := s.Deployment.Replicas(ctx)
	if err != nil {
		return err
	}

	want, reason := int32(0), scaleReasonIdle
	if now.Sub(server.LastDemandAt.Time) < s.IdleTimeout {
		want, reason = 1, scaleReasonDemand
	}

	if s.Desired != nil {
		s.Desired.WithLabelValues().Set(float64(want))
	}

	if (want == 1 && desired == 0) || (want == 0 && desired > 0) {
		if s.Fence != nil && !s.Fence.HoldsLease() {
			return nil
		}

		if err := s.Deployment.Scale(ctx, want); err != nil {
			return err
		}

		s.Log.Info("scaled the embeddings server", "from", desired, "to", want, "reason", reason)

		desired = want
	}

	state := EmbeddingsServerStarting

	switch {
	case desired == 0:
		state = EmbeddingsServerDown
	case ready > 0:
		state = EmbeddingsServerReady
	}

	if s.Fence != nil && !s.Fence.HoldsLease() {
		return nil
	}

	if err := queries.SetEmbeddingsServerState(ctx, state); err != nil {
		return fmt.Errorf("publish embeddings server state: %w", err)
	}

	return nil
}

func (s *EmbeddingsScaler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// DefaultEmbeddingsDemandInterval is the shortest time between two demand
// writes of one replica.
const DefaultEmbeddingsDemandInterval = 5 * time.Second

// EmbeddingsDemand records, from any replica, that embeddings were needed
// or soon will be.
// It writes at most once every Interval, so a busy replica adds few writes.
type EmbeddingsDemand struct {
	Pool *pgxpool.Pool
	// Interval is the shortest time between two writes. Zero means
	// DefaultEmbeddingsDemandInterval.
	Interval time.Duration
	Log      *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	written time.Time
}

// Record writes the demand unless this replica wrote one less than Interval
// ago. A failed write is logged and doesn't stop the caller; the next call
// tries again.
func (d *EmbeddingsDemand) Record(ctx context.Context) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}

	interval := d.Interval
	if interval <= 0 {
		interval = DefaultEmbeddingsDemandInterval
	}

	d.mu.Lock()
	if !d.written.IsZero() && now.Sub(d.written) < interval {
		d.mu.Unlock()

		return
	}

	previous := d.written
	d.written = now
	d.mu.Unlock()

	err := db.New(d.Pool).RecordEmbeddingsDemand(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if err != nil {
		d.mu.Lock()
		if d.written.Equal(now) {
			d.written = previous
		}
		d.mu.Unlock()

		if d.Log != nil {
			d.Log.Warn("recording embeddings demand failed", "error", err)
		}
	}
}

// EmbeddingsServerState returns the state of the embeddings server that the
// leader last published: EmbeddingsServerDown, EmbeddingsServerStarting or
// EmbeddingsServerReady.
func EmbeddingsServerState(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	server, err := db.New(pool).GetEmbeddingsServer(ctx)
	if err != nil {
		return "", fmt.Errorf("read embeddings server: %w", err)
	}

	return server.State, nil
}

// EmbeddingsServerScaledDown returns a function that reports whether the
// leader has published the bundled embeddings server as down or starting. If
// the state can't be read it reports false, so the call goes out and its own
// failure handling applies.
func EmbeddingsServerScaledDown(pool *pgxpool.Pool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		server, err := db.New(pool).GetEmbeddingsServer(ctx)
		if err != nil {
			return false
		}

		return server.State != EmbeddingsServerReady
	}
}
