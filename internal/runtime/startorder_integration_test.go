//go:build integration

package runtime_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// exchangeVolumeInstance inserts a pending instance of a template with a
// volume and an exchange, as the request that inserts it does: its render
// session is a signed-in user's, and its exchange row is closed. The attempt
// starts at the starter's clock.
func (f *volumeStartFixture) exchangeVolumeInstance(t *testing.T, started *volumeStarter, template *v1alpha1.MCPServerTemplate,
	principal string,
) db.Instance {
	t.Helper()

	session, err := f.queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: principal, SessionID: principal + "-session", Workspace: "claimed/default",
		Attestation: "user", Kind: "user", Groups: []string{},
	})
	require.NoError(t, err)

	instance := f.instance(t, template, principal)

	_, err = f.pool.Exec(t.Context(),
		"UPDATE instances SET render_session = $2, uses_exchange = true, attempt_started_at = $3 WHERE id = $1",
		instance.ID, session.ID, started.now)
	require.NoError(t, err)

	require.NoError(t, f.queries.InsertInstanceExchange(t.Context(), db.InsertInstanceExchangeParams{
		InstanceID: instance.ID, Exchange: "temporal", Closed: true,
	}))

	return f.get(t, instance.ID)
}

// exchangeVolumeTemplate is a template with one volume and a Secret that
// holds the exchanged token.
func (f *volumeStartFixture) exchangeVolumeTemplate() *v1alpha1.MCPServerTemplate {
	template := f.volumeTemplate("data")
	template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds"},"stringData":{"token":"${exchange:temporal}"}}`)}}

	return template
}

// holdVolume makes an old instance of the template hold its volume while it
// drains.
func (f *volumeStartFixture) holdVolume(t *testing.T, template *v1alpha1.MCPServerTemplate, principal string) db.Instance {
	t.Helper()

	old := f.instance(t, template, principal)
	result, err := f.store.ClaimVolumes(t.Context(), old, template.Name, claims(template, "per-user"), 0)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	f.setStatus(t, old.ID, "stopping")

	return old
}

// claimExchanges claims the instance's due exchange rows as a waiting
// request does, and returns the exchanges it may run.
func (f *volumeStartFixture) claimExchanges(t *testing.T, instanceID string) []string {
	t.Helper()

	claimed, err := f.queries.ClaimInstanceExchanges(t.Context(), db.ClaimInstanceExchangesParams{
		InstanceID: instanceID, Exchanges: []string{"temporal"},
		ClaimTimeouts: []pgtype.Interval{{Microseconds: int64(time.Minute / time.Microsecond), Valid: true}},
	})
	require.NoError(t, err)

	return claimed
}

func (f *volumeStartFixture) exchangeRows(t *testing.T, instanceID string) []db.InstanceExchange {
	t.Helper()

	rows, err := f.queries.ListInstanceExchanges(t.Context(), instanceID)
	require.NoError(t, err)

	return rows
}

// listenForInstance returns the instance notifications of the fixture's
// database once a listener listens.
func (f *volumeStartFixture) listenForInstance(t *testing.T) <-chan store.Event {
	t.Helper()

	listener := store.NewListener(f.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	return events
}

// drainUntilMarker drops the notifications sent before the marker, so the
// next one read was sent after it.
func (f *volumeStartFixture) drainUntilMarker(t *testing.T, events <-chan store.Event, marker string) {
	t.Helper()

	require.NoError(t, store.Notify(t.Context(), f.pool, store.KindSession, marker))

	deadline := time.After(30 * time.Second)

	for {
		select {
		case event := <-events:
			if event == (store.Event{Kind: store.KindSession, ID: marker}) {
				return
			}
		case <-deadline:
			t.Fatal("marker notification not received within 30s")
		}
	}
}

func TestStartOrderWithVolumesAndExchanges(t *testing.T) {
	fixture := newVolumeStartFixture(t)

	t.Run("the exchange rows stay closed while the volume waits, and the leader opens them when it is free", func(t *testing.T) {
		template := fixture.exchangeVolumeTemplate()
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))
		started.starter.Deliveries = &credentials.DeliveredTokens{Now: func() time.Time { return started.now }}
		events := fixture.listenForInstance(t)

		old := fixture.holdVolume(t, template, principal)
		instance := fixture.exchangeVolumeInstance(t, started, template, principal)

		waiting := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusPending, waiting.Status)
		assert.Equal(t, "Volume data is still held by instance "+old.ID+", which is stopping.", waiting.Reason)

		rows := fixture.exchangeRows(t, instance.ID)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].WantedSince.Valid, "the row is closed while the volume waits")
		assert.False(t, rows[0].ClaimedAt.Valid)
		assert.Equal(t, pgtype.Infinity, rows[0].RefreshAfter.InfinityModifier, "no request may claim the row")
		assert.Empty(t, fixture.claimExchanges(t, instance.ID), "no exchange runs while the volume waits")

		fixture.stop(t, old.ID)
		fixture.drainUntilMarker(t, events, instance.ID+"-before-open")

		opened := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusPending, opened.Status, "the row waits for a token")

		select {
		case event := <-events:
			assert.Equal(t, store.Event{Kind: store.KindInstance, ID: instance.ID}, event, "the leader notifies the waiting requests")
		case <-time.After(30 * time.Second):
			t.Fatal("no notification within 30s")
		}

		rows = fixture.exchangeRows(t, instance.ID)
		require.Len(t, rows, 1)
		assert.True(t, rows[0].WantedSince.Valid, "the leader opens the row once the volume is claimed")
		assert.NotEqual(t, pgtype.Infinity, rows[0].RefreshAfter.InfinityModifier)
		assert.Equal(t, []string{"temporal"}, fixture.claimExchanges(t, instance.ID), "a waiting request may now claim and exchange")

		volumes := fixture.volumeRows(t, string(template.UID))
		require.Len(t, volumes, 1)
		assert.Equal(t, pgtype.Text{String: instance.ID, Valid: true}, volumes[0].InstanceID, "the new row holds the volume")

		started.starter.Deliveries.Put(credentials.Delivery{
			Instance: instance.ID, Exchange: "temporal", Principal: principal,
			IssuedAt: started.now, ExpiresAt: started.now.Add(time.Hour), Token: credentials.Token("exchanged-token"),
		})

		starting := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusStarting, starting.Status, starting.Reason)

		var secret corev1.Secret
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: instance.Namespace, Name: "creds"}, &secret))
		assert.Equal(t, "exchanged-token", string(secret.Data["token"])+secret.StringData["token"])

		var claim corev1.PersistentVolumeClaim
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: instance.Namespace, Name: "data"}, &claim))
	})

	t.Run("a row still waiting for the volume when startTimeout passes fails with the volume reason", func(t *testing.T) {
		template := fixture.exchangeVolumeTemplate()
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		old := fixture.holdVolume(t, template, principal)
		instance := fixture.exchangeVolumeInstance(t, started, template, principal)

		started.now = started.now.Add(runtime.DefaultStartTimeout - time.Second)
		assert.Equal(t, runtime.StatusPending, step(t, started, fixture, instance.ID).Status)

		started.now = started.now.Add(time.Second)
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "Timed out waiting for volume data: Volume data is still held by instance "+old.ID+
			", which is stopping.", failed.Reason)

		rows := fixture.exchangeRows(t, instance.ID)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].WantedSince.Valid, "the rows were never opened")
	})

	t.Run("the volume wait counts toward the timeout of the token wait", func(t *testing.T) {
		template := fixture.exchangeVolumeTemplate()
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))
		started.starter.Deliveries = &credentials.DeliveredTokens{Now: func() time.Time { return started.now }}

		old := fixture.holdVolume(t, template, principal)
		instance := fixture.exchangeVolumeInstance(t, started, template, principal)
		attemptStarted := started.now

		started.now = attemptStarted.Add(3 * time.Minute)
		assert.Equal(t, runtime.StatusPending, step(t, started, fixture, instance.ID).Status, "waits for the volume")

		fixture.stop(t, old.ID)

		started.now = attemptStarted.Add(4 * time.Minute)
		assert.Equal(t, runtime.StatusPending, step(t, started, fixture, instance.ID).Status, "waits for a token")
		require.True(t, fixture.exchangeRows(t, instance.ID)[0].WantedSince.Valid, "the rows are open")

		started.now = attemptStarted.Add(runtime.DefaultStartTimeout - time.Second)
		assert.Equal(t, runtime.StatusPending, step(t, started, fixture, instance.ID).Status)

		started.now = attemptStarted.Add(runtime.DefaultStartTimeout)
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "No exchanged token for `temporal` arrived within 5m0s. Enable `"+template.Name+"` again to retry.",
			failed.Reason)
	})
}
