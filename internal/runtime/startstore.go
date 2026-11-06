package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// StartStore is what the start path reads and writes in PostgreSQL. Every
// Mark method is a compare-and-set on the row's current status: it returns
// false, and changes nothing, when the row is no longer in that status.
type StartStore interface {
	// Session returns the session row whose identity snapshot an instance
	// is rendered with.
	Session(ctx context.Context, id int64) (db.Session, error)
	// RecordSecretsUsed stores, for a pending instance, the user secrets its
	// render used and the time each was last set.
	RecordSecretsUsed(ctx context.Context, id string, versions map[string]time.Time) error
	// MarkStarting moves a pending instance to starting and records the
	// template generation its objects were applied with and the earliest
	// expiry of its resolved values (zero for none). healthCheck is the
	// rendered custom health check as JSON, nil for none.
	MarkStarting(ctx context.Context, id string, generation int64, healthCheck []byte, resolvedExpiresAt time.Time) (bool, error)
	// MarkReady moves a starting instance to ready with its tool list.
	MarkReady(ctx context.Context, id string, tools []byte) (bool, error)
	// MarkFailed moves an instance from status from to failed.
	MarkFailed(ctx context.Context, id, from, reason string) (bool, error)
	// SetHealth marks a ready instance healthy, or unhealthy with reason,
	// as HealthStore does.
	SetHealth(ctx context.Context, instance db.Instance, healthy bool, reason string) (bool, error)
}

// PostgresStartStore is the StartStore and HealthStore on PostgreSQL. Each
// status or tool list change notifies `instance:<id>` in the same
// transaction.
type PostgresStartStore struct {
	Pool *pgxpool.Pool
	// AuditOutput receives the audit events the health writes record. Nil
	// discards them; the rows are still written.
	AuditOutput io.Writer
	// DeclaresTools reports whether the template an instance runs from
	// declares its tools. The gateway then learns nothing from the
	// instance. Nil means no template declares tools.
	DeclaresTools func(instance db.Instance) bool
}

// Session implements StartStore.
func (s PostgresStartStore) Session(ctx context.Context, id int64) (db.Session, error) {
	session, err := db.New(s.Pool).GetSessionByID(ctx, id)
	if err != nil {
		return db.Session{}, fmt.Errorf("get session %d: %w", id, err)
	}

	return session, nil
}

// RecordSecretsUsed implements StartStore.
func (s PostgresStartStore) RecordSecretsUsed(ctx context.Context, id string, versions map[string]time.Time) error {
	encoded, err := EncodeSecretsUsed(versions)
	if err != nil {
		return err
	}

	// No notification: the instance is still pending and nothing else
	// needs to wake for this write.
	if _, err := db.New(s.Pool).SetInstanceSecretsUsed(ctx, db.SetInstanceSecretsUsedParams{ID: id, SecretsUsed: encoded}); err != nil {
		return fmt.Errorf("record secrets used of instance %s: %w", id, err)
	}

	return nil
}

// MarkStarting implements StartStore.
func (s PostgresStartStore) MarkStarting(ctx context.Context, id string, generation int64, healthCheck []byte, resolvedExpiresAt time.Time) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceStarting(ctx, db.SetInstanceStartingParams{
			ID: id, Generation: generation, HealthCheck: healthCheck,
			ResolvedExpiresAt: pgtype.Timestamptz{Time: resolvedExpiresAt, Valid: !resolvedExpiresAt.IsZero()},
		})
	})
}

// MarkReady implements StartStore.
func (s PostgresStartStore) MarkReady(ctx context.Context, id string, tools []byte) (bool, error) {
	return s.changeAndLearn(ctx, id, tools, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceReady(ctx, db.SetInstanceReadyParams{ID: id, Tools: tools})
	})
}

// MarkFailed implements StartStore.
func (s PostgresStartStore) MarkFailed(ctx context.Context, id, from, reason string) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{ID: id, FromStatus: from, Reason: reason})
	})
}

// Instance implements HealthStore.
func (s PostgresStartStore) Instance(ctx context.Context, id string) (db.Instance, error) {
	instance, err := db.New(s.Pool).GetInstance(ctx, id)
	if err != nil {
		return db.Instance{}, fmt.Errorf("get instance %s: %w", id, err)
	}

	return instance, nil
}

// SetTools implements HealthStore.
func (s PostgresStartStore) SetTools(ctx context.Context, id string, tools []byte) (bool, error) {
	return s.changeAndLearn(ctx, id, tools, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceTools(ctx, db.SetInstanceToolsParams{ID: id, Tools: tools})
	})
}

// SetHealth implements HealthStore. It writes instance.health, with no
// payload, in the same transaction as the compare-and-set.
func (s PostgresStartStore) SetHealth(ctx context.Context, instance db.Instance, healthy bool, reason string) (bool, error) {
	changed := false

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		var (
			rows int64
			err  error
		)

		if healthy {
			rows, err = queries.SetInstanceHealthy(ctx, instance.ID)
		} else {
			rows, err = queries.SetInstanceUnhealthy(ctx, db.SetInstanceUnhealthyParams{ID: instance.ID, Reason: reason})
		}

		if err != nil {
			return fmt.Errorf("update health of instance %s: %w", instance.ID, err)
		}

		if rows == 0 {
			return nil
		}

		changed = true

		details := map[string]any{"instance": instance.ID, "healthy": healthy, "owner": instance.OwnerPrincipal}
		if !healthy {
			details["reason"] = reason
		}

		output := s.AuditOutput
		if output == nil {
			output = io.Discard
		}

		event := audit.Event{Type: audit.TypeInstanceHealth, Server: instance.TemplateName, Details: details}
		if err := audit.NewRecorder(output, queries).Record(ctx, event); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindInstance, instance.ID)
	})

	return changed, err
}

func (s PostgresStartStore) change(ctx context.Context, id string, update func(*db.Queries) (int64, error)) (bool, error) {
	return s.changeWith(ctx, id, update, nil)
}

// changeAndLearn is change for a write of an instance's tool list. In the
// same transaction it stores the list as the catalog row of the instance's
// template generation, unless the template declares tools.
func (s PostgresStartStore) changeAndLearn(ctx context.Context, id string, tools []byte, update func(*db.Queries) (int64, error)) (bool, error) {
	return s.changeWith(ctx, id, update, func(queries *db.Queries) error {
		instance, err := queries.GetInstance(ctx, id)
		if err != nil {
			return fmt.Errorf("get instance %s: %w", id, err)
		}

		if s.DeclaresTools != nil && s.DeclaresTools(instance) {
			return nil
		}

		// The search rows of the template are rebuilt here, in the same
		// transaction, once tool search exists.
		if err := queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
			TemplateUid: instance.TemplateUid, Generation: instance.Generation,
			TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName,
			Tools: tools, InstanceID: instance.ID,
		}); err != nil {
			return fmt.Errorf("learn tools of instance %s: %w", id, err)
		}

		return nil
	})
}

func (s PostgresStartStore) changeWith(ctx context.Context, id string, update func(*db.Queries) (int64, error), afterChange func(*db.Queries) error) (bool, error) {
	changed := false

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := update(db.New(tx))
		if err != nil {
			return fmt.Errorf("update instance %s: %w", id, err)
		}

		if rows == 0 {
			return nil
		}

		changed = true

		if afterChange != nil {
			if err := afterChange(db.New(tx)); err != nil {
				return err
			}
		}

		return store.Notify(ctx, tx, store.KindInstance, id)
	})

	return changed, err
}

// EncodeSecretsUsed returns the stored form of the secrets a render used: a
// JSON object from secret name to the RFC 3339 time it was last set.
func EncodeSecretsUsed(versions map[string]time.Time) ([]byte, error) {
	stored := make(map[string]string, len(versions))
	for name, setAt := range versions {
		stored[name] = setAt.UTC().Format(time.RFC3339Nano)
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode secrets used: %w", err)
	}

	return encoded, nil
}

// DecodeSecretsUsed parses the stored form written by EncodeSecretsUsed.
func DecodeSecretsUsed(encoded []byte) (map[string]time.Time, error) {
	var stored map[string]string
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return nil, fmt.Errorf("decode secrets used: %w", err)
	}

	versions := make(map[string]time.Time, len(stored))

	for name, text := range stored {
		setAt, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, fmt.Errorf("decode secrets used: %w", err)
		}

		versions[name] = setAt
	}

	return versions, nil
}
