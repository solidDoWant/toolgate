package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// DeliveryStore is what the leader reads to check a delivered token.
type DeliveryStore interface {
	// DeliveryTarget returns the instance a delivery names and the
	// exchanges it has rows for. found is false when the row is gone.
	DeliveryTarget(ctx context.Context, id string) (instance db.Instance, exchanges []string, found bool, err error)
}

// PostgresDeliveryStore is the DeliveryStore on PostgreSQL.
type PostgresDeliveryStore struct {
	Pool *pgxpool.Pool
}

// DeliveryTarget implements DeliveryStore.
func (s PostgresDeliveryStore) DeliveryTarget(ctx context.Context, id string) (db.Instance, []string, bool, error) {
	queries := db.New(s.Pool)

	instance, err := queries.GetInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, nil, false, nil
	}

	if err != nil {
		return db.Instance{}, nil, false, fmt.Errorf("get instance %s: %w", id, err)
	}

	rows, err := queries.ListInstanceExchanges(ctx, id)
	if err != nil {
		return db.Instance{}, nil, false, fmt.Errorf("list exchanges of instance %s: %w", id, err)
	}

	exchanges := make([]string, 0, len(rows))
	for _, row := range rows {
		exchanges = append(exchanges, row.Exchange)
	}

	return instance, exchanges, true, nil
}

// DeliveryReceiver takes the exchanged tokens other replicas deliver on the
// credentials channel. Only the leader runs it.
type DeliveryReceiver struct {
	// Cipher opens the payloads.
	Cipher *secrets.Cipher
	Store  DeliveryStore
	// Tokens holds each kept token until the start or refresh applies it.
	Tokens *credentials.DeliveredTokens
	Logger *slog.Logger
}

// Receive opens payload and keeps its token when the instance is live, is
// the token's principal's own, and uses the exchange. It drops every other
// payload and logs the instance and exchange. It reports whether it kept
// the token, so the caller can start a pass.
func (r *DeliveryReceiver) Receive(ctx context.Context, payload []byte) (bool, error) {
	delivery, err := credentials.Open(r.Cipher, string(payload))
	if err != nil {
		r.logger().WarnContext(ctx, "dropped a token delivery that can't be opened")

		return false, nil
	}

	instance, exchanges, found, err := r.Store.DeliveryTarget(ctx, delivery.Instance)
	if err != nil {
		return false, err
	}

	var reason string

	switch {
	case !found:
		reason = "the instance doesn't exist"
	case !instanceLive(instance):
		reason = "the instance isn't live"
	case instance.OwnerPrincipal != delivery.Principal:
		reason = "the token is for another principal"
	case !slices.Contains(exchanges, delivery.Exchange):
		reason = "the instance doesn't use the exchange"
	}

	if reason != "" {
		r.logger().InfoContext(ctx, "dropped a token delivery: "+reason,
			slog.String("instance", delivery.Instance), slog.String("exchange", delivery.Exchange))

		return false, nil
	}

	return r.Tokens.Put(delivery), nil
}

// instanceLive reports whether an instance is starting or running and wants
// to run, so a token delivered for it may still be used.
func instanceLive(instance db.Instance) bool {
	if instance.Desired != "running" {
		return false
	}

	switch instance.Status {
	case StatusPending, StatusStarting, StatusReady:
		return true
	default:
		return false
	}
}

func (r *DeliveryReceiver) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}

	return r.Logger
}
