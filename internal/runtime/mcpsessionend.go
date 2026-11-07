package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// DefaultMCPSessionIdleTimeout is how long a connector's MCP session may go
// without use before the leader ends it, when no timeout is set.
const DefaultMCPSessionIdleTimeout = 30 * time.Minute

// connectorSessionPrefix starts the toolgate session ID of a client that
// sends no session header. Such a session lives exactly as long as its MCP
// session.
const connectorSessionPrefix = "mcp."

// The session.end reasons of a toolgate session that ends with its MCP
// session.
const (
	MCPSessionEndReasonDelete = "mcp-session-delete"
	MCPSessionEndReasonIdle   = "mcp-session-idle"
)

// MCPSessionEnder ends the connector MCP sessions that had no use for the
// idle timeout. Only the leader runs it, on its full passes. A helper's MCP
// session (one whose toolgate session came from the session header) never
// ends by idleness: it ends with its toolgate session.
type MCPSessionEnder struct {
	Pool *pgxpool.Pool
	// IdleTimeout is how long a connector MCP session may go without use.
	// Zero means DefaultMCPSessionIdleTimeout.
	IdleTimeout time.Duration
	// AuditOutput receives the session.end audit lines. Nil discards them.
	AuditOutput io.Writer
	Log         *slog.Logger
}

// Pass wraps a full pass so that idle connector MCP sessions, and their
// toolgate sessions, end before it.
func (e *MCPSessionEnder) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := e.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

// Run ends every idle connector MCP session. An error on one session doesn't
// keep the others from being handled.
func (e *MCPSessionEnder) Run(ctx context.Context) error {
	timeout := interval(e.IdleTimeout, DefaultMCPSessionIdleTimeout)

	sessions, err := db.New(e.Pool).ListIdleConnectorMCPSessions(ctx, timeout)
	if err != nil {
		return fmt.Errorf("list idle MCP sessions: %w", err)
	}

	var problems []error

	for _, session := range sessions {
		ended, err := endMCPSession(ctx, e.Pool, e.output(), session.ID, MCPSessionEndReasonIdle,
			func(queries *db.Queries) (db.McpSession, error) {
				return queries.EndIdleConnectorMCPSession(ctx, db.EndIdleConnectorMCPSessionParams{ID: session.ID, IdleTimeout: timeout})
			})
		if err != nil {
			problems = append(problems, fmt.Errorf("end idle MCP session: %w", err))

			continue
		}

		if ended && e.Log != nil {
			e.Log.InfoContext(ctx, "MCP session ended after idle timeout", slog.Int64("session", session.SessionPk))
		}
	}

	return errors.Join(problems...)
}

func (e *MCPSessionEnder) output() io.Writer {
	if e.AuditOutput == nil {
		return io.Discard
	}

	return e.AuditOutput
}

// EndMCPSession ends the live MCP session id at once: a client deleted it.
// It reports whether the session was live. An MCP session that already
// ended, or doesn't exist, is left as it is.
func EndMCPSession(ctx context.Context, pool *pgxpool.Pool, auditOutput io.Writer, id string) (bool, error) {
	if auditOutput == nil {
		auditOutput = io.Discard
	}

	ended, err := endMCPSession(ctx, pool, auditOutput, id, MCPSessionEndReasonDelete, func(queries *db.Queries) (db.McpSession, error) {
		return queries.EndLiveMCPSession(ctx, id)
	})
	if err != nil {
		return false, fmt.Errorf("end MCP session: %w", err)
	}

	return ended, nil
}

// endMCPSession is the end transaction of an MCP session. update sets
// ended_at and returns the row, or pgx.ErrNoRows when the session must not
// end. In the same transaction it notifies mcpsession:<id> and, for a
// connector session, ends the toolgate session with the given reason. It
// locks the sessions row before it touches the MCP session, the same order
// as the toolgate session's end transaction, so the two can't deadlock.
func endMCPSession(ctx context.Context, pool *pgxpool.Pool, auditOutput io.Writer, id, reason string,
	update func(queries *db.Queries) (db.McpSession, error),
) (ended bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		row, err := queries.GetMCPSession(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read MCP session: %w", err)
		}

		if _, err := queries.LockSession(ctx, row.SessionPk); err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		row, err = update(queries)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("set ended_at: %w", err)
		}

		if err := store.Notify(ctx, tx, store.KindMCPSession, row.ID); err != nil {
			return err
		}

		ended = true

		if !strings.HasPrefix(row.SessionID, connectorSessionPrefix) {
			return nil
		}

		_, err = endSessionInTx(ctx, tx, auditOutput, row.SessionPk, reason, func(queries *db.Queries) (db.Session, error) {
			return queries.EndLiveSession(ctx, row.SessionPk)
		})

		return err
	})
	if err != nil {
		return false, err
	}

	return ended, nil
}
