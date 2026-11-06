//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestRestartAfterSecretChange(t *testing.T) {
	tests := []struct {
		name      string
		secretSet bool
		usedShift time.Duration
		outdated  bool
	}{
		{name: "secret changed after the render", secretSet: true, usedShift: -time.Hour, outdated: true},
		{name: "secret unchanged since the render", secretSet: true, usedShift: 0, outdated: false},
		{name: "secret deleted after the render", secretSet: false, outdated: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMCPFixture(t)
			template := fixture.addTemplate("secret-restart", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
			id := fixture.readyInstance(t, template, "alice", template.Generation)

			current := time.Now().UTC().Truncate(time.Microsecond)
			if test.secretSet {
				_, err := fixture.pool.Exec(t.Context(),
					`INSERT INTO user_secrets (principal, name, ciphertext, key_id, updated_at) VALUES ('alice', 'token', '\x00', 1, $1)`, current)
				require.NoError(t, err)
			}

			used, err := runtime.EncodeSecretsUsed(map[string]time.Time{"token": current.Add(test.usedShift)})
			require.NoError(t, err)
			_, err = fixture.pool.Exec(t.Context(), `UPDATE instances SET secrets_used = $2 WHERE id = $1`, id, used)
			require.NoError(t, err)

			view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})

			entries, err := view.LiveInstances(t.Context(), "alice", false)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, test.outdated, entries[0].Outdated)

			err = view.RequestRestart(t.Context(), id, "alice", false)
			if test.outdated {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, gateway.ErrInstanceChanged)
			}
		})
	}
}
