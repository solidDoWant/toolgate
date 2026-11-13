// Package credentials holds the shared state rules of exchanged tokens: which
// request may claim an exchange for an instance.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// claimTimeoutMargin is added to an exchange's timeout to give its claim
// timeout, so a claim outlives the exchange that holds it.
const claimTimeoutMargin = 30 * time.Second

// Querier is the part of the generated queries that a claim uses.
type Querier interface {
	GetInstance(ctx context.Context, id string) (db.Instance, error)
	ClaimInstanceExchanges(ctx context.Context, arg db.ClaimInstanceExchangesParams) ([]string, error)
}

// Caller is the request that wants to claim. The request passes the expiry of
// its subject token, never the token.
type Caller struct {
	Principal   string
	Kind        string
	TokenExpiry time.Time
}

// Claimer claims exchanges for requests.
type Claimer struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// ClaimTimeout is the claim timeout of an exchange with the given timeout.
func ClaimTimeout(exchangeTimeout time.Duration) time.Duration {
	return exchangeTimeout + claimTimeoutMargin
}

// Claim claims the due exchanges of the instance and returns their names. It
// claims only when the instance is live and uses exchanges, and the caller is
// a user who owns the instance and whose token has not expired. Otherwise it
// returns no names. timeouts holds each configured exchange's own timeout; a
// row of an exchange that is not in it is never claimed.
func (c *Claimer) Claim(ctx context.Context, queries Querier, instanceID string, caller Caller, timeouts map[string]time.Duration) ([]string, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	if caller.Kind != auth.KindUser || caller.Principal == "" || !caller.TokenExpiry.After(now()) || len(timeouts) == 0 {
		return nil, nil
	}

	instance, err := queries.GetInstance(ctx, instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read instance: %w", err)
	}

	live := instance.Desired == "running" && instance.Status != "stopping" && instance.Status != "stopped"
	if !live || !instance.UsesExchange || instance.OwnerPrincipal != caller.Principal {
		return nil, nil
	}

	names := make([]string, 0, len(timeouts))
	claimTimeouts := make([]pgtype.Interval, 0, len(timeouts))

	for name, timeout := range timeouts {
		names = append(names, name)
		claimTimeouts = append(claimTimeouts, pgtype.Interval{Microseconds: ClaimTimeout(timeout).Microseconds(), Valid: true})
	}

	claimed, err := queries.ClaimInstanceExchanges(ctx, db.ClaimInstanceExchangesParams{
		InstanceID: instanceID, Exchanges: names, ClaimTimeouts: claimTimeouts,
	})
	if err != nil {
		return nil, fmt.Errorf("claim exchanges: %w", err)
	}

	return claimed, nil
}
