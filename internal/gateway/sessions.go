package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// lastSeenInterval is how often a request moves a session's last_seen_at
// forward when nothing else about the session changed.
const lastSeenInterval = time.Minute

// errWorkspaceChanged means a request named a different workspace than its
// live session started with (design row 22).
var errWorkspaceChanged = errors.New("this session started in a different workspace; start a new session to use another one")

// sessionStore keeps the sessions rows (docs/design/phase-1.md §2). toolgate's
// session state is keyed by (principal, X-Toolgate-Session), never by the MCP
// session.
type sessionStore struct {
	pool        *pgxpool.Pool
	auditOutput io.Writer
	now         func() time.Time
	// restore decides again and starts the servers a resumed session kept
	// (docs/design/phase-3.md §11); nil restores nothing.
	restore func(ctx context.Context, current requestContext, servers []db.SessionServer) error
}

// resolve returns the live session row for identity, creating it or
// restarting it when it ended. A live session whose stored workspace differs
// from the request's gets errWorkspaceChanged.
func (s *sessionStore) resolve(ctx context.Context, identity auth.Identity) (db.Session, error) {
	queries := db.New(s.pool)

	session, err := queries.GetSession(ctx, db.GetSessionParams{Principal: identity.Principal, SessionID: identity.Session})
	if errors.Is(err, pgx.ErrNoRows) {
		created, found, createErr := s.create(ctx, identity)
		if createErr != nil {
			return db.Session{}, createErr
		}

		if found {
			return created, nil
		}

		// Another request created it first.
		session, err = queries.GetSession(ctx, db.GetSessionParams{Principal: identity.Principal, SessionID: identity.Session})
	}

	if err != nil {
		return db.Session{}, fmt.Errorf("read session: %w", err)
	}

	if session.EndedAt.Valid {
		var kept []db.SessionServer

		session, kept, err = s.restart(ctx, session.ID, identity)
		if err != nil {
			return db.Session{}, err
		}

		if len(kept) > 0 && s.restore != nil {
			if err := s.restore(ctx, requestContext{identity: identity, session: session}, kept); err != nil {
				return db.Session{}, err
			}
		}
	}

	if session.Workspace != identity.Workspace {
		return db.Session{}, errWorkspaceChanged
	}

	return s.refresh(ctx, session, identity)
}

// create inserts the session and writes session.start in one transaction.
// found is false when the session already existed.
func (s *sessionStore) create(ctx context.Context, identity auth.Identity) (session db.Session, found bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		var createErr error

		session, createErr = queries.CreateSession(ctx, db.CreateSessionParams{
			Principal:   identity.Principal,
			SessionID:   identity.Session,
			Workspace:   identity.Workspace,
			Attestation: identity.Attestation,
			Kind:        identity.Kind,
			Email:       identity.Email,
			Groups:      snapshotGroups(identity),
		})
		if errors.Is(createErr, pgx.ErrNoRows) {
			return nil
		}

		if createErr != nil {
			return fmt.Errorf("create session: %w", createErr)
		}

		found = true

		return s.recordStart(ctx, tx, identity)
	})

	return session, found, err
}

// restart clears ended_at and takes this request's workspace and
// attestation, in one transaction holding the row lock. It keeps the enabled
// set the ended session kept only when the request's workspace and
// attestation equal the stored ones, and writes session.resume listing it.
// Otherwise it deletes the set and writes session.start. kept is the set to
// restore.
func (s *sessionStore) restart(ctx context.Context, id int64, identity auth.Identity) (session db.Session, kept []db.SessionServer, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)
		kept = nil

		locked, err := queries.LockSession(ctx, id)
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		if !locked.EndedAt.Valid {
			// Another request restarted it first.
			session = locked

			return nil
		}

		session, err = queries.RestartSession(ctx, db.RestartSessionParams{
			ID:          id,
			Workspace:   identity.Workspace,
			Attestation: identity.Attestation,
			Kind:        identity.Kind,
		})
		if err != nil {
			return fmt.Errorf("restart session: %w", err)
		}

		if locked.Workspace == identity.Workspace && locked.Attestation == identity.Attestation {
			kept, err = queries.ListSessionServers(ctx, id)
			if err != nil {
				return fmt.Errorf("read enabled servers: %w", err)
			}
		} else if err := queries.DeleteSessionServers(ctx, id); err != nil {
			return fmt.Errorf("delete enabled servers: %w", err)
		}

		if len(kept) == 0 {
			return s.recordStart(ctx, tx, identity)
		}

		servers := make([]string, 0, len(kept))
		for _, row := range kept {
			servers = append(servers, row.TemplateName)
		}

		return audit.NewRecorder(s.auditOutput, queries).Record(ctx, audit.Event{
			Type:        "session.resume",
			Principal:   identity.Principal,
			Workspace:   identity.Workspace,
			Session:     identity.Session,
			Attestation: identity.Attestation,
			Details:     map[string]any{"servers": servers},
		})
	})

	return session, kept, err
}

// refresh writes the identity snapshot when it changed, and otherwise moves
// last_seen_at forward at most once a minute.
func (s *sessionStore) refresh(ctx context.Context, session db.Session, identity auth.Identity) (db.Session, error) {
	queries := db.New(s.pool)
	groups := snapshotGroups(identity)

	if session.Email != identity.Email || !slices.Equal(session.Groups, groups) {
		updated, err := queries.UpsertSession(ctx, db.UpsertSessionParams{
			Principal:   identity.Principal,
			SessionID:   identity.Session,
			Workspace:   session.Workspace,
			Attestation: session.Attestation,
			Kind:        session.Kind,
			Email:       identity.Email,
			Groups:      groups,
		})
		if err != nil {
			return db.Session{}, fmt.Errorf("update session snapshot: %w", err)
		}

		return updated, nil
	}

	if s.now().Sub(session.LastSeenAt.Time) >= lastSeenInterval {
		if err := queries.TouchSession(ctx, session.ID); err != nil {
			return db.Session{}, fmt.Errorf("update session last_seen_at: %w", err)
		}
	}

	return session, nil
}

func (s *sessionStore) recordStart(ctx context.Context, tx pgx.Tx, identity auth.Identity) error {
	recorder := audit.NewRecorder(s.auditOutput, db.New(tx))

	return recorder.Record(ctx, audit.Event{
		Type:        "session.start",
		Principal:   identity.Principal,
		Workspace:   identity.Workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
	})
}

// snapshotGroups returns the groups to store: never nil, because the column
// is NOT NULL.
func snapshotGroups(identity auth.Identity) []string {
	if identity.Groups == nil {
		return []string{}
	}

	return identity.Groups
}
