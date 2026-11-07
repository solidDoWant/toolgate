package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// The queries of LoadPolicyState. Both filter with PostgreSQL's now(), so a
// revoke or an expiry counts at once on every replica.
const (
	liveGrantsOfPrincipalSQL = `SELECT id, principal, template_ns, template_name, tool, session_id, created_at, expires_at, revoked_at, revoked_by
FROM grants
WHERE principal = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at, id`

	liveTemporaryBlocksSQL = `SELECT id, match, reason, created_by, created_at, expires_at
FROM temporary_blocks
WHERE expires_at > now()
ORDER BY created_at, id`

	userScopeSQL = `SELECT verified_default, unverified_default, unverified_list FROM user_scopes WHERE principal = $1`

	unverifiedServersSQL = `SELECT vs.server
FROM unverified_servers vs
JOIN user_scopes us ON us.id = vs.scope_id
WHERE us.principal = $1
ORDER BY vs.server`

	workspaceEntriesSQL = `SELECT se.server, se.tool, se.effect
FROM scope_entries se
JOIN user_scopes us ON us.id = se.scope_id
WHERE us.principal = $1 AND se.workspace = $2
ORDER BY se.id`

	// effect is NULL to read both effects.
	sessionEntriesSQL = `SELECT se.server, se.tool, se.effect
FROM scope_entries se
JOIN sessions s ON s.id = se.session_pk
WHERE s.principal = $1 AND s.session_id = $2 AND ($3::text IS NULL OR se.effect = $3)
ORDER BY se.id`

	// One row per server, or one row with a NULL server for an empty list.
	workspaceListSQL = `SELECT wls.server
FROM user_scopes us
JOIN workspace_lists wl ON wl.scope_id = us.id
LEFT JOIN workspace_list_servers wls ON wls.list_id = wl.id
WHERE us.principal = $1 AND wl.workspace = $2
ORDER BY wls.server`

	sessionAdditionsSQL = `SELECT sa.server
FROM session_additions sa
JOIN sessions s ON s.id = sa.session_pk
WHERE s.principal = $1 AND s.session_id = $2
ORDER BY sa.server`
)

// ScopeQuery says which scoping rows LoadPolicyState reads. The zero value
// reads none.
type ScopeQuery struct {
	// Load reads the principal's user_scopes row.
	Load bool
	// Workspace and Session are the request's verified workspace and its
	// session ID. When Workspace is empty, the list, the workspace's
	// entries, the session's additions and its allow entries aren't read:
	// an unverified session has none of them. It reads the unverified allow
	// list and the session's deny entries instead.
	Workspace string
	Session   string
}

// PolicyRows are the rows one decision loads: the principal's live grants
// and every live temporary block, oldest first.
type PolicyRows struct {
	Grants []db.Grant
	Blocks []db.TemporaryBlock
	// Scope holds the scoping rows ScopeQuery asked for.
	Scope policy.ScopeRows
}

// LoadPolicyState reads the policy state of one request in one round trip,
// a pgx.Batch.
// withGrants false skips the grant query: grants never change the decisions
// of attested identities.
func LoadPolicyState(ctx context.Context, conn interface {
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}, principal string, withGrants bool, scope ScopeQuery,
) (PolicyRows, error) {
	var (
		rows  PolicyRows
		batch pgx.Batch
	)

	if withGrants {
		batch.Queue(liveGrantsOfPrincipalSQL, principal).Query(func(result pgx.Rows) error {
			grants, err := pgx.CollectRows(result, pgx.RowToStructByPos[db.Grant])
			rows.Grants = grants

			return err
		})
	}

	batch.Queue(liveTemporaryBlocksSQL).Query(func(result pgx.Rows) error {
		blocks, err := pgx.CollectRows(result, pgx.RowToStructByPos[db.TemporaryBlock])
		rows.Blocks = blocks

		return err
	})

	if scope.Load {
		queueScopeQueries(&batch, principal, scope, &rows.Scope)
	}

	if err := conn.SendBatch(ctx, &batch).Close(); err != nil {
		return PolicyRows{}, fmt.Errorf("load policy state: %w", err)
	}

	return rows, nil
}

// queueScopeQueries adds the scoping queries to batch, writing their results to rows. A missing
// user_scopes row leaves both defaults empty, which means none.
func queueScopeQueries(batch *pgx.Batch, principal string, scope ScopeQuery, rows *policy.ScopeRows) {
	batch.Queue(userScopeSQL, principal).Query(func(result pgx.Rows) error {
		defer result.Close()

		if result.Next() {
			if err := result.Scan(&rows.VerifiedDefault, &rows.UnverifiedDefault, &rows.UnverifiedList); err != nil {
				return err
			}
		}

		return result.Err()
	})

	if scope.Workspace == "" {
		batch.Queue(unverifiedServersSQL, principal).Query(func(result pgx.Rows) error {
			servers, err := pgx.CollectRows(result, pgx.RowTo[string])
			if len(servers) > 0 {
				rows.UnverifiedServers = servers
			}

			return err
		})

		if scope.Session != "" {
			queueEntries(batch, sessionEntriesSQL, true, rows, principal, scope.Session, policy.EffectDeny)
		}

		return
	}

	queueEntries(batch, workspaceEntriesSQL, false, rows, principal, scope.Workspace)
	queueEntries(batch, sessionEntriesSQL, true, rows, principal, scope.Session, nil)

	batch.Queue(workspaceListSQL, principal, scope.Workspace).Query(func(result pgx.Rows) error {
		servers, err := pgx.CollectRows(result, pgx.RowTo[pgtype.Text])
		if err != nil {
			return err
		}

		rows.HasList = len(servers) > 0

		for _, server := range servers {
			if server.Valid {
				rows.ListServers = append(rows.ListServers, server.String)
			}
		}

		return nil
	})

	batch.Queue(sessionAdditionsSQL, principal, scope.Session).Query(func(result pgx.Rows) error {
		additions, err := pgx.CollectRows(result, pgx.RowTo[string])
		if len(additions) > 0 {
			rows.Additions = additions
		}

		return err
	})
}

// queueEntries adds one scope entry query to batch and appends its rows to
// rows.Entries, marked as the session's when session is set.
func queueEntries(batch *pgx.Batch, query string, session bool, rows *policy.ScopeRows, arguments ...any) {
	batch.Queue(query, arguments...).Query(func(result pgx.Rows) error {
		entries, err := pgx.CollectRows(result, func(row pgx.CollectableRow) (policy.ScopeEntry, error) {
			entry := policy.ScopeEntry{Session: session}
			err := row.Scan(&entry.Server, &entry.Tool, &entry.Effect)

			return entry, err
		})
		rows.Entries = append(rows.Entries, entries...)

		return err
	})
}
