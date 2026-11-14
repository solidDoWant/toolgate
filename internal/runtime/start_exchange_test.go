package runtime_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// exchangeResource is a Secret whose token key holds the exchange's token.
func exchangeResource(name, exchange string) string {
	return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + name + `"},"stringData":{"token":"${exchange:` + exchange + `}"}}`
}

// newExchangeFixture is a starter fixture for a pending instance of
// template whose render session is a signed-in user's, with a store of
// delivered tokens.
func newExchangeFixture(t *testing.T, resources ...string) *starterFixture {
	t.Helper()

	fixture := newStarterFixture(t, testTemplate(resources...), "pending")
	session := fixture.store.sessions[7]
	session.Kind = auth.KindUser
	fixture.store.sessions[7] = session
	fixture.instance.UsesExchange = true
	fixture.starter.Deliveries = &credentials.DeliveredTokens{Now: func() time.Time { return fixture.now }}

	return fixture
}

// deliver puts a token for the fixture's instance in the leader's memory.
func deliver(fixture *starterFixture, exchange, principal, token string, issuedAt time.Time) credentials.Delivery {
	delivery := credentials.Delivery{
		Instance: fixture.instance.ID, Exchange: exchange, Principal: principal,
		IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour), Token: credentials.Token(token),
	}
	fixture.starter.Deliveries.Put(delivery)

	return delivery
}

func (m *memoryStore) exchangeRow(name string) (db.InstanceExchange, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	row, found := m.exchangeRows[name]

	return row, found
}

func (m *memoryStore) exchangeNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.exchangeRows))
	for name := range m.exchangeRows {
		names = append(names, name)
	}

	return names
}

func TestPendingExchangeTimesOut(t *testing.T) {
	tests := []struct {
		name          string
		startedBefore time.Duration
		wantStatus    string
		wantReason    string
	}{
		{
			name:          "a row with no token waits while startTimeout hasn't passed",
			startedBefore: 5*time.Minute - time.Second,
			wantStatus:    "pending",
		},
		{
			name:          "a row with no token fails once startTimeout has passed",
			startedBefore: 5 * time.Minute,
			wantStatus:    "failed",
			wantReason:    "No exchanged token for `temporal` arrived within 5m0s. Enable `github` again to retry.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))
			fixture.instance.AttemptStartedAt = pgtype.Timestamptz{Time: fixture.now.Add(-test.startedBefore), Valid: true}

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, test.wantStatus, status)
			assert.Equal(t, test.wantReason, reason)

			row, found := fixture.store.exchangeRow("temporal")
			require.True(t, found, "the leader inserts the row of the exchange the generation uses")
			assert.True(t, row.WantedSince.Valid, "the row waits for a first token")

			err := fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "credentials"}, &corev1.Secret{})
			assert.True(t, apierrors.IsNotFound(err), "nothing is applied without a token")
		})
	}

	t.Run("a token that arrives before startTimeout starts the instance", func(t *testing.T) {
		fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		require.Equal(t, "pending", status)

		deliver(fixture, "temporal", "alice", "exchanged-token", fixture.now)
		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "starting", status, reason)
	})
}

func TestPendingExchangeRowsFollowRenderedGeneration(t *testing.T) {
	issuedAt := time.Date(2026, 11, 5, 11, 59, 0, 0, time.UTC)

	t.Run("rows match the generation and the starting write records the tokens", func(t *testing.T) {
		fixture := newExchangeFixture(t, exchangeResource("temporal", "temporal"), exchangeResource("grafana", "grafana"))
		// An older generation used another exchange, and temporal already
		// waits for its token.
		wanted := pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
		fixture.store.exchangeRows = map[string]db.InstanceExchange{
			"removed":  {InstanceID: instanceID, Exchange: "removed", WantedSince: wanted},
			"temporal": {InstanceID: instanceID, Exchange: "temporal", WantedSince: wanted},
		}

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		require.Equal(t, "pending", status, "the row waits for the tokens")
		assert.ElementsMatch(t, []string{"grafana", "temporal"}, fixture.store.exchangeNames())
		assert.Equal(t, 1, fixture.store.exchangeNotifies, "a changed row notifies the waiting requests")

		row, _ := fixture.store.exchangeRow("grafana")
		assert.True(t, row.WantedSince.Valid, "the new row waits for a token")

		temporal := deliver(fixture, "temporal", "alice", "temporal-token", issuedAt)
		grafana := deliver(fixture, "grafana", "alice", "grafana-token", issuedAt.Add(time.Second))

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		require.Equal(t, "starting", status, reason)
		assert.Equal(t, 1, fixture.store.exchangeNotifies, "rows that already match change nothing")

		for name, token := range map[string]string{"temporal": "temporal-token", "grafana": "grafana-token"} {
			rendered := &corev1.Secret{}
			require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: name}, rendered))
			assert.Equal(t, token, rendered.StringData["token"])
		}

		assert.Equal(t, []runtime.RenderedExchange{
			{
				Exchange: "grafana", IssuedAt: grafana.IssuedAt, ExpiresAt: grafana.ExpiresAt,
				Targets: []runtime.ExchangeTarget{{Exchange: "grafana", Secret: "grafana", Key: "token"}},
			},
			{
				Exchange: "temporal", IssuedAt: temporal.IssuedAt, ExpiresAt: temporal.ExpiresAt,
				Targets: []runtime.ExchangeTarget{{Exchange: "temporal", Secret: "temporal", Key: "token"}},
			},
		}, fixture.store.exchanges, "the starting write records each token's targets and times")

		_, held := fixture.starter.Deliveries.Get(instanceID, "temporal")
		assert.False(t, held, "the leader forgets a token once it is recorded")
	})

	t.Run("a generation without exchanges deletes the rows", func(t *testing.T) {
		fixture := newExchangeFixture(t, configMapResource)
		fixture.store.exchangeRows = map[string]db.InstanceExchange{
			"temporal": {InstanceID: instanceID, Exchange: "temporal"},
		}

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "starting", status, reason)
		assert.Empty(t, fixture.store.exchangeNames())
		assert.Empty(t, fixture.store.exchanges, "the starting write records no exchange")
	})

	t.Run("a failed exchange for the waiting row fails the instance with its reason", func(t *testing.T) {
		fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))
		wanted := time.Now().Add(-time.Minute)
		fixture.store.exchangeRows = map[string]db.InstanceExchange{
			"temporal": {
				InstanceID: instanceID, Exchange: "temporal",
				WantedSince: pgtype.Timestamptz{Time: wanted, Valid: true},
				LastError:   pgtype.Text{String: "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`.", Valid: true},
				LastErrorAt: pgtype.Timestamptz{Time: wanted, Valid: true},
			},
		}

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "failed", status)
		assert.Equal(t, "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`.", reason)
	})

	t.Run("an error from before the row started waiting doesn't fail it", func(t *testing.T) {
		fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))
		wanted := time.Now().Add(-time.Minute)
		fixture.store.exchangeRows = map[string]db.InstanceExchange{
			"temporal": {
				InstanceID: instanceID, Exchange: "temporal",
				WantedSince: pgtype.Timestamptz{Time: wanted, Valid: true},
				LastError:   pgtype.Text{String: "an old failure", Valid: true},
				LastErrorAt: pgtype.Timestamptz{Time: wanted.Add(-time.Microsecond), Valid: true},
			},
		}

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		assert.Equal(t, "pending", status)
	})

	t.Run("a token for another principal is never rendered", func(t *testing.T) {
		fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))
		deliver(fixture, "temporal", "mallory", "foreign-token", issuedAt)

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		assert.Equal(t, "pending", status)
	})
}

func TestNonUserSnapshotFailsExchangeRender(t *testing.T) {
	for _, kind := range []string{auth.KindServiceAccount, auth.KindSPIFFE} {
		t.Run(kind+" snapshot fails at once, even with a token held", func(t *testing.T) {
			fixture := newExchangeFixture(t, exchangeResource("credentials", "temporal"))
			session := fixture.store.sessions[7]
			session.Kind = kind
			fixture.store.sessions[7] = session
			deliver(fixture, "temporal", "alice", "exchanged-token", fixture.now)

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, "failed", status)
			assert.Equal(t, "`github` needs a token exchanged from your sign-in. "+
				"Service accounts, SPIFFE workloads and pod sign-in have none, so they can't use it.", reason)

			err := fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "credentials"}, &corev1.Secret{})
			assert.True(t, apierrors.IsNotFound(err), "nothing is applied")
		})
	}

	t.Run("a non-user snapshot of a template without exchanges starts", func(t *testing.T) {
		fixture := newExchangeFixture(t, configMapResource)
		fixture.instance.UsesExchange = false
		session := fixture.store.sessions[7]
		session.Kind = auth.KindServiceAccount
		fixture.store.sessions[7] = session

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "starting", status, reason)
	})
}

// deliveryTargets is a DeliveryStore over fixed rows.
type deliveryTargets map[string]struct {
	instance  db.Instance
	exchanges []string
}

func (d deliveryTargets) DeliveryTarget(_ context.Context, id string) (db.Instance, []string, bool, error) {
	target, found := d[id]

	return target.instance, target.exchanges, found, nil
}

func TestDeliveryDroppedForStoppedOrForeignInstance(t *testing.T) {
	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	cipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	live := db.Instance{ID: instanceID, OwnerPrincipal: "alice", Desired: "running", Status: "ready"}
	issuedAt := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		instance db.Instance
		missing  bool
		rows     []string
		delivery credentials.Delivery
		wantKept bool
	}{
		{name: "a token for a live instance of its principal is kept", instance: live, rows: []string{"temporal"}, wantKept: true},
		{
			name:     "a token for a pending instance is kept",
			instance: db.Instance{ID: instanceID, OwnerPrincipal: "alice", Desired: "running", Status: "pending"},
			rows:     []string{"temporal"}, wantKept: true,
		},
		{
			name:     "a token for a stopped instance is dropped",
			instance: db.Instance{ID: instanceID, OwnerPrincipal: "alice", Desired: "stopped", Status: "stopped"},
			rows:     []string{"temporal"},
		},
		{
			name:     "a token for a stopping instance is dropped",
			instance: db.Instance{ID: instanceID, OwnerPrincipal: "alice", Desired: "stopped", Status: "stopping"},
			rows:     []string{"temporal"},
		},
		{
			name:     "a token for a failed instance is dropped",
			instance: db.Instance{ID: instanceID, OwnerPrincipal: "alice", Desired: "running", Status: "failed"},
			rows:     []string{"temporal"},
		},
		{name: "a token for a deleted instance is dropped", missing: true},
		{
			name:     "a token for another principal's instance is dropped",
			instance: db.Instance{ID: instanceID, OwnerPrincipal: "bob", Desired: "running", Status: "ready"},
			rows:     []string{"temporal"},
		},
		{name: "a token for an exchange the instance doesn't use is dropped", instance: live, rows: []string{"grafana"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targets := deliveryTargets{}
			if !test.missing {
				targets[instanceID] = struct {
					instance  db.Instance
					exchanges []string
				}{test.instance, test.rows}
			}

			var logs bytes.Buffer

			tokens := &credentials.DeliveredTokens{}
			receiver := &runtime.DeliveryReceiver{
				Cipher: cipher, Store: targets, Tokens: tokens,
				Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
			}

			payload, err := credentials.Seal(cipher, credentials.Delivery{
				Instance: instanceID, Exchange: "temporal", Principal: "alice",
				IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour), Token: credentials.Token("exchanged-token"),
			})
			require.NoError(t, err)

			kept, err := receiver.Receive(t.Context(), []byte(payload))
			require.NoError(t, err)
			assert.Equal(t, test.wantKept, kept)

			_, held := tokens.Get(instanceID, "temporal")
			assert.Equal(t, test.wantKept, held)
			assert.NotContains(t, logs.String(), "exchanged-token", "the token is never logged")

			if !test.wantKept {
				assert.Contains(t, logs.String(), `"level":"INFO"`)
				assert.Contains(t, logs.String(), `"instance":"`+instanceID+`"`)
				assert.Contains(t, logs.String(), `"exchange":"temporal"`)
			}
		})
	}

	t.Run("a payload that doesn't open is dropped", func(t *testing.T) {
		tokens := &credentials.DeliveredTokens{}
		receiver := &runtime.DeliveryReceiver{Cipher: cipher, Store: deliveryTargets{}, Tokens: tokens}

		kept, err := receiver.Receive(t.Context(), []byte(`{"v":1}`))
		require.NoError(t, err)
		assert.False(t, kept)
	})
}
