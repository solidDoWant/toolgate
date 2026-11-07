package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

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

// DefaultSessionResumeWindow is how long an ended session keeps its enabled
// set, when no window is configured.
const DefaultSessionResumeWindow = 24 * time.Hour

// SessionEnder ends idle sessions and forgets the enabled sets of sessions that ended longer ago
// than the resume window. Only the reconciler
// runs it, on its full passes. Ending a session keeps its enabled set, and
// the stop decider stops the session's per-session instances because the
// session is no longer live.
type SessionEnder struct {
	Pool *pgxpool.Pool
	// IdleTimeout is how long a session may go without requests. Zero means
	// DefaultSessionIdleTimeout.
	IdleTimeout time.Duration
	// ResumeWindow is how long an ended session keeps its enabled set. Zero
	// means DefaultSessionResumeWindow.
	ResumeWindow time.Duration
	// AuditOutput receives the session.end audit lines. Nil discards them.
	AuditOutput io.Writer
	Log         *slog.Logger
	// Metrics counts searches with no follow-up call. Nil means none.
	Metrics *Metrics
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

// Run ends every session that had no request for the idle timeout, then
// forgets the enabled set of every session that ended longer ago than the
// resume window. An error on one session doesn't keep the others from being
// handled.
func (e *SessionEnder) Run(ctx context.Context) error {
	timeout := interval(e.IdleTimeout, DefaultSessionIdleTimeout)

	var problems []error

	sessions, err := db.New(e.Pool).ListIdleSessions(ctx, timeout)
	if err != nil {
		problems = append(problems, fmt.Errorf("list idle sessions: %w", err))
	}

	for _, session := range sessions {
		if err := e.end(ctx, session.ID, timeout); err != nil {
			problems = append(problems, err)
		}
	}

	if err := e.settleSearches(ctx); err != nil {
		problems = append(problems, err)
	}

	window := interval(e.ResumeWindow, DefaultSessionResumeWindow)

	ended, err := db.New(e.Pool).ListForgettableSessions(ctx, window)
	if err != nil {
		problems = append(problems, fmt.Errorf("list ended sessions to forget: %w", err))
	}

	for _, session := range ended {
		if err := e.forget(ctx, session.ID, window); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// end ends one session in one transaction holding its row lock: it sets
// ended_at only while the session is still idle, writes session.end with
// its enabled servers and notifies session:<id>. The enabled set stays for
// the resume window. If a request moved last_seen_at forward first, it does
// nothing.
func (e *SessionEnder) end(ctx context.Context, id int64, timeout pgtype.Interval) error {
	ended, err := endSession(ctx, e.Pool, e.output(), id, "idle", func(queries *db.Queries) (db.Session, error) {
		return queries.EndIdleSession(ctx, db.EndIdleSessionParams{ID: id, IdleTimeout: timeout})
	})
	if err != nil {
		return fmt.Errorf("end idle session %d: %w", id, err)
	}

	if ended {
		e.Log.InfoContext(ctx, "session ended after idle timeout", slog.Int64("session", id))
	}

	return nil
}

// EndSession ends the live session id at once, with the same transaction as
// an idle end: ended_at, session.end with its enabled servers and reason, the
// session additions and pending requests, and a notification on
// session:<id>. The gateway uses it for a session tied to an MCP session
// that closed. A session that already ended is
// left as it is.
func EndSession(ctx context.Context, pool *pgxpool.Pool, auditOutput io.Writer, id int64, reason string) error {
	if auditOutput == nil {
		auditOutput = io.Discard
	}

	_, err := endSession(ctx, pool, auditOutput, id, reason, func(queries *db.Queries) (db.Session, error) {
		return queries.EndLiveSession(ctx, id)
	})
	if err != nil {
		return fmt.Errorf("end session %d: %w", id, err)
	}

	return nil
}

// endSession is the end transaction. update sets ended_at and returns the
// row, or pgx.ErrNoRows when the session must not end; then nothing else is
// done and ended is false.
func endSession(ctx context.Context, pool *pgxpool.Pool, auditOutput io.Writer, id int64, reason string,
	update func(queries *db.Queries) (db.Session, error),
) (ended bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ended, err = endSessionInTx(ctx, tx, auditOutput, id, reason, update)

		return err
	})

	return ended, err
}

// endSessionInTx is endSession inside the caller's transaction. It also ends
// the session's live MCP sessions and notifies mcpsession:<id> for each, so
// an MCP session never outlives its toolgate session.
func endSessionInTx(ctx context.Context, tx pgx.Tx, auditOutput io.Writer, id int64, reason string,
	update func(queries *db.Queries) (db.Session, error),
) (bool, error) {
	queries := db.New(tx)

	if _, err := queries.LockSession(ctx, id); err != nil {
		return false, fmt.Errorf("lock session: %w", err)
	}

	session, err := update(queries)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("end session: %w", err)
	}

	servers, err := enabledServerNames(ctx, queries, id)
	if err != nil {
		return false, err
	}

	err = audit.NewRecorder(auditOutput, queries).Record(ctx, audit.Event{
		Type:        "session.end",
		Principal:   session.Principal,
		Workspace:   session.Workspace,
		Session:     session.SessionID,
		Attestation: session.Attestation,
		Details:     map[string]any{"reason": reason, "servers": servers},
	})
	if err != nil {
		return false, err
	}

	if err := endSessionAdditions(ctx, tx, auditOutput, session); err != nil {
		return false, err
	}

	if err := queries.DeleteSessionFoundTools(ctx, id); err != nil {
		return false, fmt.Errorf("delete found tools: %w", err)
	}

	mcpSessions, err := queries.EndSessionMCPSessions(ctx, id)
	if err != nil {
		return false, fmt.Errorf("end MCP sessions: %w", err)
	}

	for _, mcpSession := range mcpSessions {
		if err := store.Notify(ctx, tx, store.KindMCPSession, mcpSession); err != nil {
			return false, err
		}
	}

	if err := store.Notify(ctx, tx, store.KindSession, fmt.Sprint(id)); err != nil {
		return false, err
	}

	return true, nil
}

// endSessionAdditions deletes the session additions of a session that is
// ending and expires its pending server requests, writing access.expire and
// notifying approval:<id> for each, in the ending transaction. A restart reuses the sessions row,
// so nothing of the old session may apply to it.
func endSessionAdditions(ctx context.Context, tx pgx.Tx, auditOutput io.Writer, ended db.Session) error {
	queries := db.New(tx)

	if err := queries.DeleteAllSessionAdditions(ctx, ended.ID); err != nil {
		return fmt.Errorf("delete session additions: %w", err)
	}

	// The session's deny entries stay until it is forgotten, so a resume
	// can't undo a deny.
	if err := queries.DeleteSessionScopeEntries(ctx, db.DeleteSessionScopeEntriesParams{
		SessionPk: pgtype.Int8{Int64: ended.ID, Valid: true}, Effect: "allow",
	}); err != nil {
		return fmt.Errorf("delete allow entries: %w", err)
	}

	if err := queries.DeleteSessionFoundTools(ctx, ended.ID); err != nil {
		return fmt.Errorf("delete found tools: %w", err)
	}

	expired, err := queries.ExpireSessionServerRequests(ctx, pgtype.Int8{Int64: ended.ID, Valid: true})
	if err != nil {
		return fmt.Errorf("expire server requests: %w", err)
	}

	recorder := audit.NewRecorder(auditOutput, queries)

	for _, request := range expired {
		opsmetrics.ApprovalOutcome(request.Kind, opsmetrics.OutcomeCancelled, 1)

		attestation := "user"
		if request.Verified {
			attestation = "attested"
		}

		err := recorder.Record(ctx, audit.Event{
			Type:        "access.expire",
			Principal:   request.Principal,
			Workspace:   request.Workspace,
			Session:     ended.SessionID,
			Attestation: attestation,
			Server:      request.Server.String,
			Details:     map[string]any{"request": request.ID, "kind": request.Kind},
		})
		if err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindApproval, request.ID); err != nil {
			return err
		}
	}

	return nil
}

// forget deletes the enabled set of one ended session in one transaction
// holding its row lock, and writes session.forget with the servers it
// deleted. If a request restarted the session first, or it ended again
// within the window, it does nothing.
func (e *SessionEnder) forget(ctx context.Context, id int64, window pgtype.Interval) error {
	err := pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		session, err := queries.LockSession(ctx, id)
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		servers, err := queries.ForgetEndedSessionServers(ctx, db.ForgetEndedSessionServersParams{ID: id, ResumeWindow: window})
		if err != nil {
			return fmt.Errorf("delete enabled servers: %w", err)
		}

		denies, err := queries.DeleteForgottenSessionDenyEntries(ctx, db.DeleteForgottenSessionDenyEntriesParams{ID: id, ResumeWindow: window})
		if err != nil {
			return fmt.Errorf("delete deny entries: %w", err)
		}

		if len(servers) == 0 && denies == 0 {
			return nil
		}

		slices.Sort(servers)

		err = audit.NewRecorder(e.output(), queries).Record(ctx, audit.Event{
			Type:        "session.forget",
			Principal:   session.Principal,
			Workspace:   session.Workspace,
			Session:     session.SessionID,
			Attestation: session.Attestation,
			Details:     map[string]any{"servers": nonNilNames(servers), "denyEntries": denies},
		})
		if err != nil {
			return err
		}

		e.Log.InfoContext(ctx, "ended session's enabled servers forgotten", slog.Int64("session", id))

		return nil
	})
	if err != nil {
		return fmt.Errorf("forget ended session %d: %w", id, err)
	}

	return nil
}

func (e *SessionEnder) output() io.Writer {
	if e.AuditOutput == nil {
		return io.Discard
	}

	return e.AuditOutput
}

// enabledServerNames lists the names of the servers the session has enabled.
func enabledServerNames(ctx context.Context, queries *db.Queries, id int64) ([]string, error) {
	rows, err := queries.ListSessionServers(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read enabled servers: %w", err)
	}

	servers := make([]string, 0, len(rows))
	for _, row := range rows {
		servers = append(servers, row.TemplateName)
	}

	return servers, nil
}

func interval(value, fallback time.Duration) pgtype.Interval {
	if value == 0 {
		value = fallback
	}

	return pgtype.Interval{Microseconds: value.Microseconds(), Valid: true}
}

func nonNilNames(names []string) []string {
	if names == nil {
		return []string{}
	}

	return names
}
