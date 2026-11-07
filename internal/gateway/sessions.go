package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// lastSeenInterval is how often a request moves a session's last_seen_at
// forward when nothing else about the session changed.
const lastSeenInterval = time.Minute

// errWorkspaceChanged means a request named a different workspace than its
// live session started with.
var errWorkspaceChanged = errors.New("this session started in a different workspace; start a new session to use another one")

// errSessionEnded means a request named a session tied to an MCP session
// (auth.MCPSessionPrefix) that has ended. Such a session ends with its MCP
// session and is never resumed.
var errSessionEnded = errors.New("this session ended with its MCP session")

// sessionStore keeps the sessions rows. toolgate's
// session state is keyed by (principal, X-Toolgate-Session), never by the MCP
// session.
type sessionStore struct {
	pool        *pgxpool.Pool
	auditOutput io.Writer
	now         func() time.Time
	// restore decides again and starts the servers a resumed session kept,
	// inside the restart's transaction tx; nil
	// restores nothing.
	restore func(ctx context.Context, tx pgx.Tx, current requestContext, servers []db.SessionServer) error
}

// resolve returns the live session row for identity, creating it or
// restarting it when it ended. A live session whose stored workspace differs
// from the request's gets errWorkspaceChanged. An ended session tied to an
// MCP session gets errSessionEnded: a request that raced the end of its MCP
// session must not resume it.
func (s *sessionStore) resolve(ctx context.Context, identity auth.Identity, searchMode bool) (db.Session, error) {
	queries := db.New(s.pool)

	session, err := queries.GetSession(ctx, db.GetSessionParams{Principal: identity.Principal, SessionID: identity.Session})
	if errors.Is(err, pgx.ErrNoRows) {
		created, found, createErr := s.create(ctx, identity, searchMode)
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
		if strings.HasPrefix(identity.Session, auth.MCPSessionPrefix) {
			return db.Session{}, errSessionEnded
		}

		session, err = s.restart(ctx, session.ID, identity)
		if err != nil {
			return db.Session{}, err
		}
	}

	if session.Workspace != identity.Workspace {
		return db.Session{}, errWorkspaceChanged
	}

	return s.refresh(ctx, session, identity, searchMode)
}

// create inserts the session and writes session.start in one transaction.
// found is false when the session already existed.
func (s *sessionStore) create(ctx context.Context, identity auth.Identity, searchMode bool) (session db.Session, found bool, err error) {
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
			ToolSearch:  searchMode,
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
// Otherwise it deletes the set and writes session.start. The kept set is
// restored in the same transaction, so a restore that fails part-way leaves
// the session ended and the next request tries the whole restart again.
func (s *sessionStore) restart(ctx context.Context, id int64, identity auth.Identity) (session db.Session, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		var kept []db.SessionServer

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

		// A 0.3 leader ends a session without clearing its allow entries,
		// found tools or pending requests. Nothing of the old session may
		// apply to the restarted one; deny entries stay.
		if err := s.clearEndedState(ctx, tx, locked); err != nil {
			return err
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

		if err := audit.NewRecorder(s.auditOutput, queries).Record(ctx, audit.Event{
			Type:        "session.resume",
			Principal:   identity.Principal,
			Workspace:   identity.Workspace,
			Session:     identity.Session,
			Attestation: identity.Attestation,
			Details:     map[string]any{"servers": servers},
		}); err != nil {
			return err
		}

		if s.restore == nil {
			return nil
		}

		return s.restore(ctx, tx, requestContext{identity: identity, session: session}, kept)
	})

	return session, err
}

// clearEndedState deletes the allow entries and found tools of an ended
// session that is restarting, and expires its pending server and scope
// requests, writing access.expire for each. Ending in 0.4 already did this, so
// it only changes sessions a 0.3 leader ended.
func (s *sessionStore) clearEndedState(ctx context.Context, tx pgx.Tx, ended db.Session) error {
	queries := db.New(tx)
	sessionPK := pgtype.Int8{Int64: ended.ID, Valid: true}

	if err := queries.DeleteSessionScopeEntries(ctx, db.DeleteSessionScopeEntriesParams{SessionPk: sessionPK, Effect: "allow"}); err != nil {
		return fmt.Errorf("delete allow entries: %w", err)
	}

	if err := queries.DeleteSessionFoundTools(ctx, ended.ID); err != nil {
		return fmt.Errorf("delete found tools: %w", err)
	}

	expired, err := queries.ExpireSessionServerRequests(ctx, sessionPK)
	if err != nil {
		return fmt.Errorf("expire session requests: %w", err)
	}

	recorder := audit.NewRecorder(s.auditOutput, queries)

	for _, request := range expired {
		attestation := "user"
		if request.Verified {
			attestation = "attested"
		}

		if err := recorder.Record(ctx, audit.Event{
			Type:        "access.expire",
			Principal:   request.Principal,
			Workspace:   request.Workspace,
			Session:     ended.SessionID,
			Attestation: attestation,
			Server:      request.Server.String,
			Details:     map[string]any{"request": request.ID, "kind": request.Kind},
		}); err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindApproval, request.ID); err != nil {
			return err
		}
	}

	return nil
}

// refresh writes the identity snapshot when it changed, and otherwise moves
// last_seen_at forward at most once a minute. A change of the tool search
// mode is written at once.
func (s *sessionStore) refresh(ctx context.Context, session db.Session, identity auth.Identity, searchMode bool) (db.Session, error) {
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
			ToolSearch:  searchMode,
		})
		if err != nil {
			return db.Session{}, fmt.Errorf("update session snapshot: %w", err)
		}

		return updated, nil
	}

	if session.ToolSearch != searchMode || s.now().Sub(session.LastSeenAt.Time) >= lastSeenInterval {
		session.ToolSearch = searchMode

		if err := queries.TouchSession(ctx, db.TouchSessionParams{ID: session.ID, ToolSearch: searchMode}); err != nil {
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
