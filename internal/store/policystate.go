package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

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
)

// PolicyRows are the rows one decision loads: the principal's live grants
// and every live temporary block, oldest first.
type PolicyRows struct {
	Grants []db.Grant
	Blocks []db.TemporaryBlock
}

// LoadPolicyState reads the policy state of one request in one round trip,
// a pgx.Batch (docs/design/phase-2.md §3, "Loading grants and blocks").
// withGrants false skips the grant query: grants never change the decisions
// of attested identities.
func LoadPolicyState(ctx context.Context, conn interface {
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}, principal string, withGrants bool,
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

	if err := conn.SendBatch(ctx, &batch).Close(); err != nil {
		return PolicyRows{}, fmt.Errorf("load grants and temporary blocks: %w", err)
	}

	return rows, nil
}
