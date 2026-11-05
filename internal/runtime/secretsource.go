package runtime

import (
	"context"
	"time"
)

// SecretGetter reads one of a principal's secrets (secrets.Broker.Get).
type SecretGetter func(ctx context.Context, principal, name string) ([]byte, time.Time, error)

// BrokerSecrets returns, for each principal, the SecretSource that reads
// that principal's secrets through get.
func BrokerSecrets(get SecretGetter) func(principal string) SecretSource {
	return func(principal string) SecretSource {
		return brokerSource{get: get, principal: principal}
	}
}

type brokerSource struct {
	get       SecretGetter
	principal string
}

func (b brokerSource) Secret(ctx context.Context, name string) (string, error) {
	value, _, err := b.get(ctx, b.principal, name)
	if err != nil {
		return "", err
	}

	return string(value), nil
}
