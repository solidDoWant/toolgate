package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// DefaultSessionIdleTimeout is how long a session may go without requests
// before the reconciler ends it, when no timeout is configured.
const DefaultSessionIdleTimeout = 2 * time.Hour

// SessionEnder ends idle sessions (docs/design/phase-1.md §2, "Ending a
// session"). Only the reconciler runs it, on its full passes. Ending a
// session deletes its enabled set, so the stop decider then stops the
// session's per-session instances.
type SessionEnder struct {
	Pool *pgxpool.Pool
	// IdleTimeout is how long a session may go without requests. Zero means
	// DefaultSessionIdleTimeout.
	IdleTimeout time.Duration
	// AuditOutput receives the session.end audit lines. Nil discards them.
	AuditOutput io.Writer
	Log         *slog.Logger
}

// Pass wraps a full pass so that idle sessions are ended before it, and
// their instances are judged in the same pass.
func (e *SessionEnder) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := e.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

// Run ends every session that had no request for the idle timeout. An error
// on one session doesn't keep the others from being ended.
func (e *SessionEnder) Run(ctx context.Context) error {
	timeout := e.interval()

	sessions, err := db.New(e.Pool).ListIdleSessions(ctx, timeout)
	if err != nil {
		return fmt.Errorf("list idle sessions: %w", err)
	}

	var problems []error

	for _, session := range sessions {
		if err := e.end(ctx, session.ID, timeout); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// end ends one session in one transaction holding its row lock: it sets
// ended_at only while the session is still idle, deletes its enabled set,
// writes session.end and notifies session:<id>. If a request moved
// last_seen_at forward first, it does nothing.
func (e *SessionEnder) end(ctx context.Context, id int64, timeout pgtype.Interval) error {
	err := pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.LockSession(ctx, id); err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		ended, err := queries.EndIdleSession(ctx, db.EndIdleSessionParams{ID: id, IdleTimeout: timeout})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("end session: %w", err)
		}

		if err := queries.DeleteSessionServers(ctx, id); err != nil {
			return fmt.Errorf("delete enabled servers: %w", err)
		}

		output := e.AuditOutput
		if output == nil {
			output = io.Discard
		}

		err = audit.NewRecorder(output, queries).Record(ctx, audit.Event{
			Type:        "session.end",
			Principal:   ended.Principal,
			Workspace:   ended.Workspace,
			Session:     ended.SessionID,
			Attestation: ended.Attestation,
			Details:     map[string]any{"reason": "idle"},
		})
		if err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindSession, fmt.Sprint(id)); err != nil {
			return err
		}

		e.Log.InfoContext(ctx, "session ended after idle timeout", slog.Int64("session", id))

		return nil
	})
	if err != nil {
		return fmt.Errorf("end idle session %d: %w", id, err)
	}

	return nil
}

func (e *SessionEnder) interval() pgtype.Interval {
	timeout := e.IdleTimeout
	if timeout == 0 {
		timeout = DefaultSessionIdleTimeout
	}

	return pgtype.Interval{Microseconds: timeout.Microseconds(), Valid: true}
}
