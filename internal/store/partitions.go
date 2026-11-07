package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Audit partition upkeep settings.
const (
	// DefaultAuditPartitionInterval is how often the leader creates the
	// audit partitions for the current and the next month.
	DefaultAuditPartitionInterval = 24 * time.Hour
	// auditPartitionMaxBackoff bounds the delay between retries of a failed
	// call.
	auditPartitionMaxBackoff = 5 * time.Minute
)

// AuditPartitionKeeper calls toolgate_ensure_audit_partitions() when it
// starts and every Interval after that, so a gateway that runs for months
// without a restart always has a partition for the current month. It runs
// only on the replica holding the leader Lease. A failed call is logged,
// counted in toolgate_audit_partition_errors_total, and retried with
// backoff until it succeeds.
type AuditPartitionKeeper struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Interval between successful calls. Zero means
	// DefaultAuditPartitionInterval.
	Interval time.Duration
	// RetryDelay is the first delay after a failed call. It doubles on each
	// failure up to 5 minutes. Zero means one second.
	RetryDelay time.Duration

	errors prometheus.Counter
}

// NewAuditPartitionKeeper returns a keeper for pool.
func NewAuditPartitionKeeper(pool *pgxpool.Pool, log *slog.Logger) *AuditPartitionKeeper {
	return &AuditPartitionKeeper{
		Pool: pool,
		Log:  log,
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toolgate_audit_partition_errors_total",
			Help: "Failed attempts of the leader to create the audit partitions for the current and the next month.",
		}),
	}
}

// Errors returns the counter of failed calls.
func (k *AuditPartitionKeeper) Errors() prometheus.Counter { return k.errors }

// NeedLeaderElection makes the manager run the keeper only on the Lease
// holder.
func (k *AuditPartitionKeeper) NeedLeaderElection() bool { return true }

// Start calls the function at once and then every Interval until ctx ends.
func (k *AuditPartitionKeeper) Start(ctx context.Context) error {
	interval := k.Interval
	if interval <= 0 {
		interval = DefaultAuditPartitionInterval
	}

	firstRetry := k.RetryDelay
	if firstRetry <= 0 {
		firstRetry = time.Second
	}

	retry := firstRetry

	for {
		wait := interval

		if err := EnsureAuditPartitions(ctx, k.Pool); err != nil {
			if ctx.Err() != nil {
				return nil
			}

			k.errors.Inc()
			k.Log.Error("could not create the audit partitions; retrying", "error", err, "retryIn", retry)

			wait = retry
			retry = min(retry*2, auditPartitionMaxBackoff)
		} else {
			retry = firstRetry
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil
		case <-timer.C:
		}
	}
}
