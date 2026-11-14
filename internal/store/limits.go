package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Row limits.
const (
	// MaxLiveGrantsPerPrincipal is the most live grants one principal has.
	MaxLiveGrantsPerPrincipal = 100
	// MaxLiveTemporaryBlocks is the most live temporary blocks.
	MaxLiveTemporaryBlocks = 1000
	// MaxPendingApprovalsPerPrincipal is the most pending approvals one
	// principal has.
	MaxPendingApprovalsPerPrincipal = 100
	// MaxPersistentVolumeProblemBytes is the longest persistent volume
	// problem text.
	MaxPersistentVolumeProblemBytes = 1024
	// PersistentVolumePageSize is the rows in one page of the admin volume
	// list.
	PersistentVolumePageSize = 200
)

// ErrLimitReached is returned by an insert that would pass its row limit.
var ErrLimitReached = errors.New("store: row limit reached")

// InsertGrant inserts a grant unless the principal already has
// MaxLiveGrantsPerPrincipal live grants; then it returns ErrLimitReached.
// params.LiveLimit is set by InsertGrant. The principal's advisory lock is
// taken before the count, so concurrent inserts can't both pass it.
func InsertGrant(ctx context.Context, pool *pgxpool.Pool, params db.InsertGrantWithinLimitParams) (db.Grant, error) {
	return InsertGrantThen(ctx, pool, params, nil)
}

// InsertGrantThen is InsertGrant that also runs then, if not nil, in the
// insert's transaction after the grant is inserted. An error from then
// rolls the insert back.
func InsertGrantThen(ctx context.Context, pool *pgxpool.Pool, params db.InsertGrantWithinLimitParams,
	then func(tx pgx.Tx, grant db.Grant) error,
) (db.Grant, error) {
	params.LiveLimit = MaxLiveGrantsPerPrincipal

	return insertWithinLimit(ctx, pool, "grant",
		func(queries *db.Queries) error { return queries.LockPrincipalGrants(ctx, params.Principal) },
		func(queries *db.Queries) (db.Grant, error) { return queries.InsertGrantWithinLimit(ctx, params) },
		then)
}

// InsertTemporaryBlock inserts a temporary block unless
// MaxLiveTemporaryBlocks are already live; then it returns ErrLimitReached.
// params.LiveLimit is set by InsertTemporaryBlock.
func InsertTemporaryBlock(ctx context.Context, pool *pgxpool.Pool, params db.InsertTemporaryBlockWithinLimitParams) (db.TemporaryBlock, error) {
	params.LiveLimit = MaxLiveTemporaryBlocks

	return insertWithinLimit(ctx, pool, "temporary block",
		func(queries *db.Queries) error { return queries.LockTemporaryBlockInsert(ctx) },
		func(queries *db.Queries) (db.TemporaryBlock, error) {
			return queries.InsertTemporaryBlockWithinLimit(ctx, params)
		}, nil)
}

// InsertApproval inserts a pending approval unless the principal already
// has MaxPendingApprovalsPerPrincipal pending approvals; then it returns
// ErrLimitReached. params.PendingLimit is set by InsertApproval.
func InsertApproval(ctx context.Context, pool *pgxpool.Pool, params db.InsertApprovalWithinLimitParams) (db.Approval, error) {
	params.PendingLimit = MaxPendingApprovalsPerPrincipal

	return insertWithinLimit(ctx, pool, "approval",
		func(queries *db.Queries) error { return queries.LockPrincipalApprovals(ctx, params.Principal) },
		func(queries *db.Queries) (db.Approval, error) { return queries.InsertApprovalWithinLimit(ctx, params) }, nil)
}

// insertWithinLimit runs lock, then insert, in one transaction. The insert
// is a separate statement, so under READ COMMITTED it counts every row
// committed before the lock was granted. An insert that returns no row
// passed its limit. then, if not nil, runs last in the same transaction.
func insertWithinLimit[Row any](ctx context.Context, pool *pgxpool.Pool, kind string,
	lock func(*db.Queries) error, insert func(*db.Queries) (Row, error), then func(pgx.Tx, Row) error,
) (Row, error) {
	var row Row

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := lock(queries); err != nil {
			return fmt.Errorf("lock %s insert: %w", kind, err)
		}

		inserted, err := insert(queries)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLimitReached
		}

		if err != nil {
			return fmt.Errorf("insert %s: %w", kind, err)
		}

		row = inserted

		if then != nil {
			return then(tx, inserted)
		}

		return nil
	})

	return row, err
}
