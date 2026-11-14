package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// ExchangeRefresher applies refreshed exchanged tokens to running instances.
// It patches the Secret keys each exchange row recorded at render, so no
// pod restarts and the template is never read.
type ExchangeRefresher struct {
	Pool *pgxpool.Pool
	// Client patches instance Secrets.
	Client client.Writer
	// Tokens holds the delivered tokens.
	Tokens *credentials.DeliveredTokens
	Log    *slog.Logger
}

// Step applies the held tokens of one starting or ready instance.
func (r *ExchangeRefresher) Step(ctx context.Context, instance db.Instance) error {
	if r.Tokens == nil || instance.Desired != desiredRunning || !instance.UsesExchange ||
		(instance.Status != StatusStarting && instance.Status != statusReady) {
		return nil
	}

	queries := db.New(r.Pool)

	rows, err := queries.ListInstanceExchanges(ctx, instance.ID)
	if err != nil {
		return fmt.Errorf("list exchanges of instance %s: %w", instance.ID, err)
	}

	for _, row := range rows {
		delivery, found := r.Tokens.Get(instance.ID, row.Exchange)
		if !found {
			continue
		}

		if delivery.Principal != instance.OwnerPrincipal {
			r.Tokens.Forget(instance.ID, row.Exchange, delivery.IssuedAt)

			continue
		}

		if err := r.apply(ctx, queries, instance, row, delivery); err != nil {
			return err
		}
	}

	return nil
}

// apply patches one row's targets with the delivered token and records the
// new times. A token that isn't newer than the row's, or whose patch fails,
// is forgotten and leaves the row as it is.
func (r *ExchangeRefresher) apply(ctx context.Context, queries *db.Queries, instance db.Instance,
	row db.InstanceExchange, delivery credentials.Delivery,
) error {
	defer r.Tokens.Forget(instance.ID, row.Exchange, delivery.IssuedAt)

	if row.IssuedAt.Valid && !delivery.IssuedAt.After(row.IssuedAt.Time) {
		return nil
	}

	var targets []exchangeTarget
	if len(row.Targets) > 0 {
		if err := json.Unmarshal(row.Targets, &targets); err != nil {
			return fmt.Errorf("instance %s: read targets of exchange %s: %w", instance.ID, row.Exchange, err)
		}
	}

	values := make([]SecretKeyValue, 0, len(targets))
	for _, target := range targets {
		values = append(values, SecretKeyValue{
			Namespace: instance.Namespace, Secret: target.Secret, Key: target.Key, Value: []byte(delivery.Token),
		})
	}

	for _, value := range values {
		if err := PatchSecretKeys(ctx, r.Client, []SecretKeyValue{value}); err != nil {
			r.logger().WarnContext(ctx, "couldn't patch an exchanged token into an instance Secret",
				slog.String("instance", instance.ID), slog.String("secret", value.Secret), slog.String("key", value.Key),
				slog.String("error", err.Error()))

			return nil
		}
	}

	_, err := queries.ApplyInstanceExchange(ctx, db.ApplyInstanceExchangeParams{
		InstanceID: instance.ID, Exchange: row.Exchange,
		IssuedAt:     pgtype.Timestamptz{Time: delivery.IssuedAt, Valid: true},
		ExpiresAt:    pgtype.Timestamptz{Time: delivery.ExpiresAt, Valid: true},
		RefreshAfter: pgtype.Timestamptz{Time: RefreshAfter(delivery.IssuedAt, delivery.ExpiresAt), Valid: true},
		OldIssuedAt:  row.IssuedAt,
	})
	if err != nil {
		return fmt.Errorf("instance %s: record refreshed exchange %s: %w", instance.ID, row.Exchange, err)
	}

	return nil
}

func (r *ExchangeRefresher) logger() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}

	return r.Log
}
