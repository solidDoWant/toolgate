package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

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

// PayloadRetentionStore lists and deletes old audit rows by key, so their
// payload objects can be deleted first. *db.Queries implements it.
type PayloadRetentionStore interface {
	ListExpiredAuditEvents(ctx context.Context, arg db.ListExpiredAuditEventsParams) ([]db.ListExpiredAuditEventsRow, error)
	DeleteAuditEventsByKey(ctx context.Context, arg db.DeleteAuditEventsByKeyParams) (int64, error)
}

// ObjectDeleter deletes payload objects and returns the keys it couldn't
// delete. *Payloads implements it.
type ObjectDeleter interface {
	DeleteObjects(ctx context.Context, keys []string) (map[string]bool, error)
}

// Retention deletes audit rows older than Age, in batches of BatchSize.
// With Objects set, it deletes each batch's payload objects before the rows,
// and keeps the rows whose object S3 refused to delete for the next run.
type Retention struct {
	Store     RetentionStore
	Age       time.Duration
	BatchSize int32
	Interval  time.Duration
	// Rows and Objects, when both are set, make the job delete payload
	// objects with their rows.
	Rows    PayloadRetentionStore
	Objects ObjectDeleter
	// Refused counts rows kept because their object couldn't be deleted.
	// It may be nil.
	Refused prometheus.Counter
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
		Refused: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toolgate_audit_retention_refused_total",
			Help: "Expired audit rows kept for a later run because S3 refused to delete their payload objects.",
		}),
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

	if r.Rows != nil && r.Objects != nil {
		return r.deleteWithObjects(ctx, cutoff)
	}

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

// deleteWithObjects pages through the expired rows with an (occurred_at,
// id) cursor. For each page it deletes the payload objects, then the rows
// whose object is gone. Rows whose object S3 refused are kept and counted,
// and the cursor moves past them.
func (r *Retention) deleteWithObjects(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	after := pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}

	var (
		afterID      int64
		total        int64
		deleteErrors []error
	)

	for {
		rows, err := r.Rows.ListExpiredAuditEvents(ctx, db.ListExpiredAuditEventsParams{
			Cutoff:          cutoff,
			AfterOccurredAt: after,
			AfterID:         afterID,
			BatchSize:       r.BatchSize,
		})
		if err != nil {
			return total, fmt.Errorf("list expired audit events: %w", err)
		}

		if len(rows) == 0 {
			return total, errors.Join(deleteErrors...)
		}

		var keys []string

		for _, row := range rows {
			if row.PayloadRef != "" {
				keys = append(keys, row.PayloadRef)
			}
		}

		refused := map[string]bool{}

		if len(keys) > 0 {
			refused, err = r.Objects.DeleteObjects(ctx, keys)
			if err != nil {
				deleteErrors = append(deleteErrors, err)
			}
		}

		params := db.DeleteAuditEventsByKeyParams{}

		var kept int

		for _, row := range rows {
			if row.PayloadRef != "" && refused[row.PayloadRef] {
				kept++

				continue
			}

			params.OccurredAts = append(params.OccurredAts, row.OccurredAt)
			params.Ids = append(params.Ids, row.ID)
		}

		if kept > 0 {
			if r.Refused != nil {
				r.Refused.Add(float64(kept))
			}

			if r.Logger != nil {
				r.Logger.WarnContext(ctx, "audit retention kept rows whose payload objects could not be deleted",
					slog.Int("rows", kept))
			}
		}

		if len(params.Ids) > 0 {
			deleted, err := r.Rows.DeleteAuditEventsByKey(ctx, params)
			total += deleted

			if err != nil {
				return total, fmt.Errorf("delete expired audit events: %w", err)
			}
		}

		last := rows[len(rows)-1]
		after, afterID = last.OccurredAt, last.ID

		if len(rows) < int(r.BatchSize) {
			return total, errors.Join(deleteErrors...)
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
