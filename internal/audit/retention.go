package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

const (
	// DefaultRetention is how long audit rows are kept when audit.retention
	// isn't set.
	DefaultRetention = 90 * 24 * time.Hour
	// DefaultRetentionBatchSize is how many rows one delete statement
	// removes at most.
	DefaultRetentionBatchSize = 1000
	// DefaultRetentionInterval is how often the retention job runs.
	DefaultRetentionInterval = time.Hour
)

// RetentionStore deletes old audit rows. *db.Queries implements it.
type RetentionStore interface {
	DeleteAuditEventsBefore(ctx context.Context, arg db.DeleteAuditEventsBeforeParams) (int64, error)
}

// Retention deletes audit rows older than Age, in batches of BatchSize.
type Retention struct {
	Store     RetentionStore
	Age       time.Duration
	BatchSize int32
	Interval  time.Duration
	// Now returns the current time; it defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// NewRetention returns a Retention job with the default batch size and
// interval that keeps rows for age, or DefaultRetention when age is zero.
func NewRetention(store RetentionStore, age time.Duration, logger *slog.Logger) *Retention {
	if age <= 0 {
		age = DefaultRetention
	}

	return &Retention{
		Store:     store,
		Age:       age,
		BatchSize: DefaultRetentionBatchSize,
		Interval:  DefaultRetentionInterval,
		Now:       time.Now,
		Logger:    logger,
	}
}

// DeleteExpired deletes every row older than Age, one batch per statement,
// and returns how many rows it deleted.
func (r *Retention) DeleteExpired(ctx context.Context) (int64, error) {
	if r.Age <= 0 || r.BatchSize <= 0 {
		return 0, fmt.Errorf("delete expired audit events: age and batch size must be positive")
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	cutoff := pgtype.Timestamptz{Time: now().Add(-r.Age), Valid: true}

	var total int64

	for {
		deleted, err := r.Store.DeleteAuditEventsBefore(ctx, db.DeleteAuditEventsBeforeParams{
			Cutoff:    cutoff,
			BatchSize: r.BatchSize,
		})
		total += deleted

		if err != nil {
			return total, fmt.Errorf("delete expired audit events: %w", err)
		}

		if deleted < int64(r.BatchSize) {
			return total, nil
		}
	}
}

// Run calls DeleteExpired at once and then every Interval until ctx is
// done. Errors are logged and retried at the next interval.
func (r *Retention) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultRetentionInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if _, err := r.DeleteExpired(ctx); err != nil && ctx.Err() == nil && r.Logger != nil {
			r.Logger.ErrorContext(ctx, "audit retention failed", slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
