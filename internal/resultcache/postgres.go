package resultcache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

const (
	// deleteBatchRows is the most rows one delete statement removes, so a
	// large clean-up never holds locks for long.
	deleteBatchRows = 1000
	// deleteStatementsPerKind is the most delete statements of one kind in a
	// pass. The next pass deletes the rest.
	deleteStatementsPerKind = 10
)

// Postgres is the Store that keeps entries in the tool_result_cache table.
type Postgres struct {
	pool             *pgxpool.Pool
	maxEntryBytes    int
	maxTemplateBytes int64
}

var _ Store = (*Postgres)(nil)

// NewPostgres returns a store with the entry size limit and the template
// size limit, both in bytes.
func NewPostgres(pool *pgxpool.Pool, maxEntryBytes int, maxTemplateBytes int64) *Postgres {
	return &Postgres{pool: pool, maxEntryBytes: maxEntryBytes, maxTemplateBytes: maxTemplateBytes}
}

// Get returns the unexpired entry for key. Every key column is checked, and
// the age is read from the database's clock.
func (p *Postgres) Get(ctx context.Context, key Key) (Hit, bool, error) {
	hash := key.Hash()

	row, err := db.New(p.pool).GetToolResultCache(ctx, db.GetToolResultCacheParams{
		Key: hash[:], TemplateUid: key.TemplateUID, Generation: key.Generation, Tool: key.Tool, ScopeID: key.ScopeID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Hit{}, false, nil
	}

	if err != nil {
		return Hit{}, false, fmt.Errorf("read cached result: %w", err)
	}

	return Hit{Result: row.Result, Age: intervalDuration(row.Age)}, true, nil
}

// Put stores result for key for ttl. A per-session entry is stored only
// while its session is live, and an entry is stored only when the
// template's unexpired entries stay within the template limit. Nothing is
// evicted.
func (p *Postgres) Put(ctx context.Context, key Key, result []byte, ttl time.Duration) (PutResult, error) {
	if len(result) > p.maxEntryBytes {
		return PutEntryTooLarge, nil
	}

	var sessionPK pgtype.Int8

	if key.Scope == templatescope.PerSession {
		id, err := strconv.ParseInt(key.ScopeID, 10, 64)
		if err != nil {
			return "", fmt.Errorf("per-session scope ID %q is not a session row ID: %w", key.ScopeID, err)
		}

		sessionPK = pgtype.Int8{Int64: id, Valid: true}
	}

	hash := key.Hash()
	stored := PutStored

	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if sessionPK.Valid {
			if _, err := queries.LockLiveSessionForResultCache(ctx, sessionPK.Int64); errors.Is(err, pgx.ErrNoRows) {
				stored = PutSessionEnded

				return nil
			} else if err != nil {
				return fmt.Errorf("lock session: %w", err)
			}
		}

		if err := queries.LockToolResultCacheTemplate(ctx, key.TemplateUID); err != nil {
			return fmt.Errorf("lock template: %w", err)
		}

		used, err := queries.SumToolResultCacheTemplateBytes(ctx, db.SumToolResultCacheTemplateBytesParams{
			TemplateUid: key.TemplateUID, Key: hash[:],
		})
		if err != nil {
			return fmt.Errorf("sum template bytes: %w", err)
		}

		if used+int64(len(result)) > p.maxTemplateBytes {
			stored = PutTemplateFull

			return nil
		}

		err = queries.UpsertToolResultCache(ctx, db.UpsertToolResultCacheParams{
			Key: hash[:], TemplateUid: key.TemplateUID, Generation: key.Generation, Tool: key.Tool,
			Scope: key.Scope, ScopeID: key.ScopeID, SessionPk: sessionPK, Result: result,
			SizeBytes: int32(len(result)), //nolint:gosec // bounded by maxEntryBytes, at most 16 MiB.
			Ttl:       pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("store result: %w", err)
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("store cached result: %w", err)
	}

	return stored, nil
}

// DeleteTemplate deletes the entries of a template UID of every generation
// except keep, in batches. keep 0 deletes them all. It runs at most
// deleteStatementsPerKind statements.
func (p *Postgres) DeleteTemplate(ctx context.Context, templateUID string, keep int64) error {
	queries := db.New(p.pool)

	err := deleteBatches(ctx, func(ctx context.Context) (int64, error) {
		return queries.DeleteToolResultCacheTemplate(ctx, db.DeleteToolResultCacheTemplateParams{TemplateUid: templateUID, Keep: keep})
	})
	if err != nil {
		return fmt.Errorf("delete cached results of template %s: %w", templateUID, err)
	}

	return nil
}

// Cleanup deletes expired entries, the entries of template UIDs and
// generations that current does not map, and the per-session entries of
// ended sessions. A failed delete does not stop the others.
func (p *Postgres) Cleanup(ctx context.Context, current map[string]int64) error {
	queries := db.New(p.pool)

	var problems []error

	if err := deleteBatches(ctx, queries.DeleteExpiredToolResultCache); err != nil {
		problems = append(problems, fmt.Errorf("delete expired cached results: %w", err))
	}

	pairs, err := queries.ListToolResultCacheTemplates(ctx)
	if err != nil {
		problems = append(problems, fmt.Errorf("list cached templates: %w", err))
	}

	// A UID whose pairs are all stale is deleted once, with keep 0.
	done := map[string]bool{}

	for _, pair := range pairs {
		generation, known := current[pair.TemplateUid]
		if known && generation == pair.Generation {
			continue
		}

		if done[pair.TemplateUid] {
			continue
		}

		done[pair.TemplateUid] = true

		if err := p.DeleteTemplate(ctx, pair.TemplateUid, generation); err != nil {
			problems = append(problems, err)
		}
	}

	if err := deleteBatches(ctx, queries.DeleteEndedSessionToolResultCache); err != nil {
		problems = append(problems, fmt.Errorf("delete cached results of ended sessions: %w", err))
	}

	return errors.Join(problems...)
}

// DeleteSessionEntries deletes the per-session entries of one session. It
// takes the caller's transaction, so the delete commits or rolls back with
// the end of the session.
func DeleteSessionEntries(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	if err := db.New(tx).DeleteSessionToolResultCache(ctx, pgtype.Int8{Int64: sessionID, Valid: true}); err != nil {
		return fmt.Errorf("delete cached results of session: %w", err)
	}

	return nil
}

// deleteBatches runs the batch delete until a batch is not full, or the
// per-pass statement limit is reached.
func deleteBatches(ctx context.Context, deleteBatch func(context.Context) (int64, error)) error {
	for range deleteStatementsPerKind {
		rows, err := deleteBatch(ctx)
		if err != nil {
			return err
		}

		if rows < deleteBatchRows {
			return nil
		}
	}

	return nil
}

func intervalDuration(interval pgtype.Interval) time.Duration {
	const day = 24 * time.Hour

	return time.Duration(interval.Microseconds)*time.Microsecond +
		time.Duration(interval.Days)*day + time.Duration(interval.Months)*30*day
}
