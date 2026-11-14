package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
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
	// rendered custom health check as JSON, nil for none. templateSecrets
	// are the sources of its template secret values, written in the same
	// transaction. exchanges are the tokens the render used; the same
	// write sets uses_exchange from them and records each one's targets
	// and times on its instance_exchanges row.
	MarkStarting(ctx context.Context, id string, generation int64, healthCheck []byte, resolvedExpiresAt time.Time,
		templateSecrets []TemplateSecretTarget, exchanges []RenderedExchange) (bool, error)
	// SyncExchanges makes a pending instance's instance_exchanges rows match
	// the exchanges used by the generation it renders, and returns them. It
	// inserts a waiting row for each used exchange that has none, deletes
	// the rows of the others, and marks each row in unheld that isn't
	// waiting yet as wanted now. It notifies `instance:<id>` in the same
	// transaction when a row changed, so waiting requests claim.
	SyncExchanges(ctx context.Context, id string, used, unheld []string) ([]db.InstanceExchange, error)
	// MarkReady moves a starting instance to ready with its tool list and
	// the MCP revision it speaks, and records instance.ready. The revision
	// never changes while the row lives.
	MarkReady(ctx context.Context, instance db.Instance, tools []byte, revision string) (bool, error)
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
	// RunningTemplate returns the last valid generation of the template an
	// instance runs from. Each learned list rebuilds the template's search
	// rows from it. Nil means no rows are rebuilt here.
	RunningTemplate func(instance db.Instance) (*v1alpha1.MCPServerTemplate, bool)
	// Metrics counts starts and health failures. Nil means none.
	Metrics *Metrics
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

// RenderedExchange is an exchanged token a render used: where it went and
// its times. It never holds the token.
type RenderedExchange struct {
	Exchange  string
	Targets   []ExchangeTarget
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// RefreshAfter is when a request must exchange a token again: half-way
// between its issue and its expiry.
func RefreshAfter(issuedAt, expiresAt time.Time) time.Time {
	return issuedAt.Add(expiresAt.Sub(issuedAt) / 2)
}

// exchangeTarget is the stored form of one target in instance_exchanges.
type exchangeTarget struct {
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

// MarkStarting implements StartStore.
func (s PostgresStartStore) MarkStarting(ctx context.Context, id string, generation int64, healthCheck []byte, resolvedExpiresAt time.Time,
	templateSecrets []TemplateSecretTarget, exchanges []RenderedExchange,
) (bool, error) {
	return s.changeWith(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceStarting(ctx, db.SetInstanceStartingParams{
			ID: id, Generation: generation, HealthCheck: healthCheck, UsesExchange: len(exchanges) > 0,
			ResolvedExpiresAt: pgtype.Timestamptz{Time: resolvedExpiresAt, Valid: !resolvedExpiresAt.IsZero()},
		})
	}, func(_ pgx.Tx, queries *db.Queries) error {
		if err := writeRenderedExchanges(ctx, queries, id, exchanges); err != nil {
			return err
		}

		return writeTemplateSecrets(ctx, queries, id, templateSecrets)
	})
}

// writeRenderedExchanges records, for each token a render used, where it is
// and its times. The row no longer waits and nobody holds its claim.
func writeRenderedExchanges(ctx context.Context, queries *db.Queries, id string, exchanges []RenderedExchange) error {
	for _, exchange := range exchanges {
		targets := make([]exchangeTarget, 0, len(exchange.Targets))
		for _, target := range exchange.Targets {
			targets = append(targets, exchangeTarget{Secret: target.Secret, Key: target.Key})
		}

		encoded, err := json.Marshal(targets)
		if err != nil {
			return fmt.Errorf("record exchange %s of instance %s: %w", exchange.Exchange, id, err)
		}

		if err := queries.SetInstanceExchangeRendered(ctx, db.SetInstanceExchangeRenderedParams{
			InstanceID: id, Exchange: exchange.Exchange, Targets: encoded,
			IssuedAt:     pgtype.Timestamptz{Time: exchange.IssuedAt, Valid: true},
			ExpiresAt:    pgtype.Timestamptz{Time: exchange.ExpiresAt, Valid: true},
			RefreshAfter: pgtype.Timestamptz{Time: RefreshAfter(exchange.IssuedAt, exchange.ExpiresAt), Valid: true},
		}); err != nil {
			return fmt.Errorf("record exchange %s of instance %s: %w", exchange.Exchange, id, err)
		}
	}

	return nil
}

// SyncExchanges implements StartStore.
func (s PostgresStartStore) SyncExchanges(ctx context.Context, id string, used, unheld []string) ([]db.InstanceExchange, error) {
	var rows []db.InstanceExchange

	// A nil list would be SQL NULL, and NOT (exchange = ANY (NULL)) keeps
	// every row.
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		existing, err := queries.ListInstanceExchanges(ctx, id)
		if err != nil {
			return fmt.Errorf("list exchanges of instance %s: %w", id, err)
		}

		changed := false

		for _, exchange := range used {
			if slices.ContainsFunc(existing, func(row db.InstanceExchange) bool { return row.Exchange == exchange }) {
				continue
			}

			if err := queries.InsertInstanceExchange(ctx, db.InsertInstanceExchangeParams{
				InstanceID: id, Exchange: exchange, Wanted: true,
			}); err != nil {
				return fmt.Errorf("insert exchange %s of instance %s: %w", exchange, id, err)
			}

			changed = true
		}

		deleted, err := queries.DeleteInstanceExchangesNotIn(ctx, db.DeleteInstanceExchangesNotInParams{InstanceID: id, Keep: append([]string{}, used...)})
		if err != nil {
			return fmt.Errorf("delete exchanges of instance %s: %w", id, err)
		}

		wanted, err := queries.MarkInstanceExchangesWantedIn(ctx, db.MarkInstanceExchangesWantedInParams{InstanceID: id, Exchanges: unheld})
		if err != nil {
			return fmt.Errorf("mark exchanges of instance %s wanted: %w", id, err)
		}

		if changed || deleted > 0 || wanted > 0 {
			if err := store.Notify(ctx, tx, store.KindInstance, id); err != nil {
				return err
			}
		}

		rows, err = queries.ListInstanceExchanges(ctx, id)
		if err != nil {
			return fmt.Errorf("list exchanges of instance %s: %w", id, err)
		}

		return nil
	})

	return rows, err
}

// writeTemplateSecrets replaces an instance's instance_template_secrets rows
// with the sources of its latest render. The rows never hold a value.
func writeTemplateSecrets(ctx context.Context, queries *db.Queries, id string, targets []TemplateSecretTarget) error {
	if err := queries.DeleteInstanceTemplateSecrets(ctx, id); err != nil {
		return fmt.Errorf("record template secrets of instance %s: %w", id, err)
	}

	for _, target := range targets {
		if err := queries.InsertInstanceTemplateSecret(ctx, db.InsertInstanceTemplateSecretParams{
			InstanceID: id, TargetSecret: target.TargetSecret, TargetKey: target.TargetKey, TargetField: target.TargetField,
			SourceNamespace: target.SourceNamespace, SourceName: target.SourceName, SourceKey: target.SourceKey,
			SourceVersion: target.SourceVersion,
		}); err != nil {
			return fmt.Errorf("record template secrets of instance %s: %w", id, err)
		}
	}

	return nil
}

// MarkReady implements StartStore. It writes instance.ready, with no
// payload, in the same transaction as the compare-and-set.
func (s PostgresStartStore) MarkReady(ctx context.Context, instance db.Instance, tools []byte, revision string) (bool, error) {
	changed, err := s.changeAndLearn(ctx, instance.ID, tools, func(queries *db.Queries) (int64, error) {
		rows, err := queries.SetInstanceReady(ctx, db.SetInstanceReadyParams{ID: instance.ID, Tools: tools, McpRevision: revision})
		if err != nil || rows == 0 {
			return rows, err
		}

		output := s.AuditOutput
		if output == nil {
			output = io.Discard
		}

		event := audit.Event{Type: audit.TypeInstanceReady, Server: instance.TemplateName, Details: map[string]any{
			"instance": instance.ID, "owner": instance.OwnerPrincipal, "mcpRevision": revision,
		}}

		return rows, audit.NewRecorder(output, queries).Record(ctx, event)
	})
	if changed && err == nil {
		s.Metrics.observeReady(instance.TemplateName, instance.CreatedAt.Time, time.Now())
	}

	return changed, err
}

// MarkFailed implements StartStore.
func (s PostgresStartStore) MarkFailed(ctx context.Context, id, from, reason string) (bool, error) {
	changed, err := s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{ID: id, FromStatus: from, Reason: reason})
	})
	// A ready instance that fails is a repair that gave up, counted there.
	if changed && err == nil && s.Metrics != nil && from != StatusReady {
		if instance, getErr := db.New(s.Pool).GetInstance(ctx, id); getErr == nil {
			s.Metrics.observeStart(instance.TemplateName, StartFailed)
		}
	}

	return changed, err
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

	if changed && err == nil && !healthy {
		s.Metrics.observeHealthFailure(instance.TemplateName)
	}

	return changed, err
}

func (s PostgresStartStore) change(ctx context.Context, id string, update func(*db.Queries) (int64, error)) (bool, error) {
	return s.changeWith(ctx, id, update, nil)
}

// changeAndLearn is change for a write of an instance's tool list. In the
// same transaction it stores the list as the catalog row of the instance's
// template generation, unless the template declares tools or the instance
// is a remote server's.
func (s PostgresStartStore) changeAndLearn(ctx context.Context, id string, tools []byte, update func(*db.Queries) (int64, error)) (bool, error) {
	return s.changeWith(ctx, id, update, func(tx pgx.Tx, queries *db.Queries) error {
		instance, err := queries.GetInstance(ctx, id)
		if err != nil {
			return fmt.Errorf("get instance %s: %w", id, err)
		}

		// A remote server's tool list comes from one user's credential, so
		// it stays on that user's row and is never shared as the catalog's.
		if IsRemote(instance) || (s.DeclaresTools != nil && s.DeclaresTools(instance)) {
			return nil
		}

		if err := queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
			TemplateUid: instance.TemplateUid, Generation: instance.Generation,
			TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName,
			Tools: tools, InstanceID: instance.ID,
		}); err != nil {
			return fmt.Errorf("learn tools of instance %s: %w", id, err)
		}

		if s.RunningTemplate == nil {
			return nil
		}

		template, found := s.RunningTemplate(instance)
		if !found {
			return nil
		}

		return RebuildToolSearch(ctx, tx, instance.TemplateUid, template)
	})
}

func (s PostgresStartStore) changeWith(ctx context.Context, id string, update func(*db.Queries) (int64, error), afterChange func(pgx.Tx, *db.Queries) error) (bool, error) {
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
			if err := afterChange(tx, db.New(tx)); err != nil {
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
