package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// heldExchanges are the delivered tokens a render uses, by exchange name.
type heldExchanges map[string]credentials.Delivery

// renderTokens returns the tokens as the render takes them.
func (h heldExchanges) renderTokens() map[string]string {
	if len(h) == 0 {
		return nil
	}

	tokens := make(map[string]string, len(h))
	for name, delivery := range h {
		tokens[name] = string(delivery.Token)
	}

	return tokens
}

// rendered returns what the starting write records for each token, with
// the Secret keys the render put it in.
func (h heldExchanges) rendered(targets []ExchangeTarget) []RenderedExchange {
	if len(h) == 0 {
		return nil
	}

	exchanges := make([]RenderedExchange, 0, len(h))

	for _, name := range slices.Sorted(maps.Keys(h)) {
		delivery := h[name]
		exchange := RenderedExchange{Exchange: name, IssuedAt: delivery.IssuedAt, ExpiresAt: delivery.ExpiresAt}

		for _, target := range targets {
			if target.Exchange == name {
				exchange.Targets = append(exchange.Targets, target)
			}
		}

		exchanges = append(exchanges, exchange)
	}

	return exchanges
}

// NoUserTokenMessage is the reason an instance of server fails when its
// render needs an exchanged token and its identity isn't a signed-in user.
func NoUserTokenMessage(server string) string {
	return "`" + server + "` needs a token exchanged from your sign-in. " +
		"Service accounts, SPIFFE workloads and pod sign-in have none, so they can't use it."
}

// prepareExchanges is the exchange step of a pending start. It returns the
// tokens to render with and true when the render may go on. It returns
// false when the row waits for a token or has failed.
func (s *Starter) prepareExchanges(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	identity Identity,
) (heldExchanges, bool, error) {
	used := catalog.ExchangeNames(template)

	// A generation without exchanges has no rows to keep, unless an older
	// generation inserted some.
	if len(used) == 0 && !instance.UsesExchange {
		return nil, true, nil
	}

	if len(used) > 0 && identity.Kind != auth.KindUser {
		return nil, false, s.fail(ctx, instance, StatusPending, NoUserTokenMessage(template.Name))
	}

	held := heldExchanges{}

	var unheld []string

	for _, name := range used {
		delivery, found := s.deliveredToken(instance, name)
		if !found {
			unheld = append(unheld, name)

			continue
		}

		held[name] = delivery
	}

	rows, err := s.Store.SyncExchanges(ctx, instance.ID, used, unheld)
	if err != nil {
		return nil, false, fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	for _, row := range rows {
		if row.LastError.Valid && row.LastErrorAt.Valid && row.WantedSince.Valid && !row.LastErrorAt.Time.Before(row.WantedSince.Time) {
			return nil, false, s.fail(ctx, instance, StatusPending, row.LastError.String)
		}
	}

	if len(unheld) == 0 {
		return held, true, nil
	}

	if s.timedOut(instance, template) {
		return nil, false, s.fail(ctx, instance, StatusPending, fmt.Sprintf(
			"No exchanged token for `%s` arrived within %s. Enable `%s` again to retry.",
			unheld[0], startTimeout(template), template.Name))
	}

	return nil, false, nil
}

// deliveredToken returns the token held for an instance's exchange. A
// token for another principal is never used.
func (s *Starter) deliveredToken(instance db.Instance, exchange string) (credentials.Delivery, bool) {
	if s.Deliveries == nil {
		return credentials.Delivery{}, false
	}

	delivery, found := s.Deliveries.Get(instance.ID, exchange)
	if !found || delivery.Principal != instance.OwnerPrincipal {
		return credentials.Delivery{}, false
	}

	return delivery, true
}

// forgetExchanges drops the tokens the starting write recorded.
func (s *Starter) forgetExchanges(instance db.Instance, held heldExchanges) {
	if s.Deliveries == nil {
		return
	}

	for name, delivery := range held {
		s.Deliveries.Forget(instance.ID, name, delivery.IssuedAt)
	}
}
