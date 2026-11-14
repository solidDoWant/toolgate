//go:build integration

package runtime_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const exchangeSecretValue = "old-token"

var exchangeEveryone = v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"}

// exchangeInstance makes a live instance of a per-user template with one
// exchange row whose token (issued at issuedAt, valid for lifetime) is in the
// Secret `creds`, key `token`, of the instance namespace.
func (f *stopFixture) exchangeInstance(t *testing.T, kube client.Client, name, status string, issuedAt time.Time, lifetime time.Duration,
) (*v1alpha1.MCPServerTemplate, db.Instance) {
	t.Helper()

	template := f.template(name, "per-user", time.Hour, exchangeEveryone)
	session := f.session(t, name+"-user", "s1", "", "staff")
	f.enable(t, session, template)

	instance := f.instance(t, template, "user:"+name+"-user", name+"-user", status)

	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET uses_exchange = true, last_used_at = now() WHERE id = $1", instance.ID)
	require.NoError(t, err)
	require.NoError(t, f.queries.InsertInstanceExchange(t.Context(), db.InsertInstanceExchangeParams{
		InstanceID: instance.ID, Exchange: "temporal", Wanted: true,
	}))
	_, err = f.pool.Exec(t.Context(), `UPDATE instance_exchanges SET targets = '[{"secret":"creds","key":"token"}]',
		issued_at = $2, expires_at = $3, refresh_after = $4, wanted_since = NULL, claimed_at = now(),
		last_error = 'earlier failure', last_error_at = now() WHERE instance_id = $1`,
		instance.ID, issuedAt, issuedAt.Add(lifetime), issuedAt.Add(lifetime/2))
	require.NoError(t, err)

	require.NoError(t, kube.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: "creds"},
		Data:       map[string][]byte{"token": []byte(exchangeSecretValue)},
	}))

	return template, f.get(t, instance.ID)
}

func (f *stopFixture) exchangeRow(t *testing.T, instance db.Instance) db.InstanceExchange {
	t.Helper()

	rows, err := f.queries.ListInstanceExchanges(t.Context(), instance.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	return rows[0]
}

func (f *stopFixture) secretToken(t *testing.T, kube client.Client, instance db.Instance) string {
	t.Helper()

	secret := &corev1.Secret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "creds"}, secret))

	return string(secret.Data["token"])
}

func (f *stopFixture) refresher(kube client.Client, tokens *credentials.DeliveredTokens) *runtime.ExchangeRefresher {
	return &runtime.ExchangeRefresher{Pool: f.pool, Client: kube, Tokens: tokens, Log: slog.New(slog.NewTextHandler(f.logs, nil))}
}

func deliveryFor(instance db.Instance, token string, issuedAt time.Time, lifetime time.Duration) credentials.Delivery {
	return credentials.Delivery{
		Instance: instance.ID, Exchange: "temporal", Principal: instance.OwnerPrincipal,
		IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(lifetime), Token: credentials.Token(token),
	}
}

func newExchangeKube() client.Client { return clientfake.NewClientBuilder().Build() }

func TestRefreshDropsOlderDelivery(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	issued := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
	_, instance := fixture.exchangeInstance(t, kube, "refresh-older", "ready", issued, time.Hour)
	before := fixture.exchangeRow(t, instance)

	tokens := &credentials.DeliveredTokens{}
	refresher := fixture.refresher(kube, tokens)

	for name, delivered := range map[string]time.Time{"older": issued.Add(-time.Minute), "same time": issued} {
		tokens.Put(deliveryFor(instance, "stale-token", delivered, time.Hour))
		require.NoError(t, refresher.Step(t.Context(), instance), name)

		assert.Equal(t, exchangeSecretValue, fixture.secretToken(t, kube, instance), name)
		assert.Equal(t, before, fixture.exchangeRow(t, instance), name)

		_, held := tokens.Get(instance.ID, "temporal")
		assert.False(t, held, "%s: the token is forgotten", name)
	}

	// A later delivery is applied: it proves the drops above came from the time check.
	fresh := issued.Add(time.Minute)
	tokens.Put(deliveryFor(instance, "fresh-token", fresh, 2*time.Hour))
	require.NoError(t, refresher.Step(t.Context(), instance))

	assert.Equal(t, "fresh-token", fixture.secretToken(t, kube, instance))

	row := fixture.exchangeRow(t, instance)
	assert.True(t, row.IssuedAt.Time.Equal(fresh))
	assert.True(t, row.ExpiresAt.Time.Equal(fresh.Add(2*time.Hour)))
	assert.True(t, row.RefreshAfter.Time.Equal(fresh.Add(time.Hour)), "refresh at half-life")
	assert.False(t, row.ClaimedAt.Valid)
	assert.False(t, row.LastError.Valid)
	assert.False(t, row.LastErrorAt.Valid)
}

func TestRefreshUsesRenderedTargets(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	issued := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)

	// The template in the catalog has no resources at all: it changed since
	// the instance rendered.
	template, instance := fixture.exchangeInstance(t, kube, "refresh-targets", "starting", issued, time.Hour)
	require.Empty(t, template.Spec.Resources)

	tokens := &credentials.DeliveredTokens{}
	tokens.Put(deliveryFor(instance, "new-token", issued.Add(time.Minute), time.Hour))

	require.NoError(t, fixture.refresher(kube, tokens).Step(t.Context(), instance))
	assert.Equal(t, "new-token", fixture.secretToken(t, kube, instance))
}

func TestRefreshPatchFailureKeepsRow(t *testing.T) {
	fixture := newStopFixture(t)
	issued := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)

	kube := clientfake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("namespace is being deleted")
		},
	}).Build()

	_, instance := fixture.exchangeInstance(t, kube, "refresh-failure", "ready", issued, time.Hour)
	before := fixture.exchangeRow(t, instance)

	tokens := &credentials.DeliveredTokens{}
	tokens.Put(deliveryFor(instance, "new-token", issued.Add(time.Minute), time.Hour))

	require.NoError(t, fixture.refresher(kube, tokens).Step(t.Context(), instance))

	assert.Equal(t, before, fixture.exchangeRow(t, instance), "the row keeps its times, claim and error")
	assert.Equal(t, exchangeSecretValue, fixture.secretToken(t, kube, instance))

	_, held := tokens.Get(instance.ID, "temporal")
	assert.False(t, held, "the token is forgotten")
	assert.Contains(t, fixture.logs.String(), instance.ID)
	assert.Contains(t, fixture.logs.String(), "creds")
	assert.Contains(t, fixture.logs.String(), "token")
	assert.NotContains(t, fixture.logs.String(), "new-token")
}

func TestExpiredExchangeStopsEnabledInstance(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	now := time.Now().Truncate(time.Microsecond)
	fixture.setNow(now)

	_, live := fixture.exchangeInstance(t, kube, "expiry-live", "ready", now.Add(-30*time.Minute), time.Hour)
	_, expired := fixture.exchangeInstance(t, kube, "expiry-expired", "ready", now.Add(-2*time.Hour), time.Hour)

	fixture.pass(t)

	current := fixture.get(t, live.ID)
	assert.Equal(t, "running", current.Desired)
	assert.False(t, current.CredentialExpired)

	current = fixture.get(t, expired.ID)
	assert.Equal(t, "stopped", current.Desired, "a session that enables the server doesn't keep it")
	assert.True(t, current.CredentialExpired)
	assert.Contains(t, fixture.logs.String(), "its exchanged token `temporal` expired")
	fixture.requireOneStopEvent(t, expired, "credential-expired")
}

func TestUnconfiguredExchangeStopsAtExpiry(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	now := time.Now().Truncate(time.Microsecond)
	fixture.setNow(now)

	// The template has no exchange configured any more, so nothing refreshes
	// the token.
	template, instance := fixture.exchangeInstance(t, kube, "expiry-unconfigured", "ready", now.Add(-50*time.Minute), time.Hour)
	require.Empty(t, template.Spec.Resources)

	fixture.pass(t)
	assert.Equal(t, "running", fixture.get(t, instance.ID).Desired, "it runs until the token expires")

	fixture.setNow(now.Add(10*time.Minute + time.Second))
	fixture.pass(t)

	current := fixture.get(t, instance.ID)
	assert.Equal(t, "stopped", current.Desired)
	assert.True(t, current.CredentialExpired)
}

func TestNewLeaderStopsOnRowExpiry(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	now := time.Now().Truncate(time.Microsecond)
	fixture.setNow(now)

	_, instance := fixture.exchangeInstance(t, kube, "expiry-new-leader", "ready", now.Add(-time.Hour), 90*time.Minute)

	// Each pass builds its deciders from nothing, as a leader that just took
	// the Lease does: only the row carries the expiry.
	fixture.pass(t)
	assert.Equal(t, "running", fixture.get(t, instance.ID).Desired)

	fixture.setNow(now.Add(30*time.Minute + time.Second))
	fixture.pass(t)
	assert.Equal(t, "stopped", fixture.get(t, instance.ID).Desired)
	assert.True(t, fixture.get(t, instance.ID).CredentialExpired)
}

// exchangeTemplate sets a resource that uses the exchange, and persistent
// volumes when volumes is true.
func exchangeTemplate(template *v1alpha1.MCPServerTemplate, volumes bool) {
	template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds"},"stringData":{"token":"${exchange:temporal}"}}`)}}
	if volumes {
		template.Spec.PersistentVolumes = []v1alpha1.PersistentVolume{{Name: "cache", Size: "1Gi"}}
	}
}

func TestReplacementWaitsForExchangedToken(t *testing.T) {
	for name, volumes := range map[string]bool{"without volumes": false, "with volumes": true} {
		t.Run(name, func(t *testing.T) {
			fixture := newStopFixture(t)
			kube := newExchangeKube()
			now := time.Now().Truncate(time.Microsecond)
			fixture.setNow(now)

			template, old := fixture.exchangeInstance(t, kube, "replacement-waits", "ready", now.Add(-10*time.Minute), time.Hour)
			exchangeTemplate(template, volumes)
			fixture.catalog.Set(template, nil)
			fixture.requestRestart(t, old, "replacement-waits-user")

			require.NoError(t, fixture.replacer().Step(t.Context(), fixture.get(t, old.ID)))

			replacement, found := fixture.liveInstance(t, old)
			require.True(t, found)
			require.NotEqual(t, old.ID, replacement.ID)
			assert.Equal(t, "pending", replacement.Status)
			assert.True(t, replacement.UsesExchange)

			row := fixture.exchangeRow(t, replacement)
			assert.False(t, row.IssuedAt.Valid, "it holds no token")
			assert.False(t, row.ClaimedAt.Valid)

			if volumes {
				assert.False(t, row.WantedSince.Valid, "closed until the volume steps are done")
				assert.True(t, row.RefreshAfter.InfinityModifier == pgtype.Infinity, "refresh_after is infinity")
			} else {
				assert.True(t, row.WantedSince.Valid, "it waits for a token")
				assert.False(t, row.RefreshAfter.Time.After(time.Now()), "a request may claim it")
			}
		})
	}
}

func TestReapplyReplacesInstanceWithExchange(t *testing.T) {
	fixture := newStopFixture(t)
	kube := newExchangeKube()
	now := time.Now().Truncate(time.Microsecond)
	fixture.setNow(now)

	template, instance := fixture.exchangeInstance(t, kube, "reapply-exchange", "ready", now.Add(-10*time.Minute), time.Hour)
	exchangeTemplate(template, false)
	fixture.catalog.Set(template, nil)

	unhealthy := fixture.makeUnhealthy(t, instance, healthCheck("reapply", 3), now.Add(-time.Second))
	require.True(t, unhealthy.UsesExchange)

	require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), unhealthy))

	assert.Equal(t, "stopped", fixture.get(t, instance.ID).Desired)
	assert.Equal(t, 1, fixture.healthStops(t, instance.ID), "the stop has reason health-check")
	assert.Zero(t, fixture.auditCount(t, "instance.reapply", instance.ID))

	replacement, found := fixture.liveInstance(t, instance)
	require.True(t, found)
	assert.NotEqual(t, instance.ID, replacement.ID)
}

func TestSweepMarksExchangeWanted(t *testing.T) {
	for name, volumes := range map[string]bool{"without volumes": false, "with volumes": true} {
		t.Run(name, func(t *testing.T) {
			fixture := newStopFixture(t)
			kube := newExchangeKube()
			now := time.Now().Truncate(time.Microsecond)

			template, instance := fixture.exchangeInstance(t, kube, "sweep-exchange", "ready", now.Add(-10*time.Minute), time.Hour)
			exchangeTemplate(template, volumes)
			fixture.catalog.Set(template, nil)
			require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Namespace}}))

			fixture.sweep(t, fixture.kube)

			current := fixture.get(t, instance.ID)
			require.Equal(t, "pending", current.Status)

			row := fixture.exchangeRow(t, current)
			assert.False(t, row.ClaimedAt.Valid, "the claim is cleared")

			if volumes {
				assert.False(t, row.WantedSince.Valid)
				assert.True(t, row.RefreshAfter.InfinityModifier == pgtype.Infinity, "refresh_after is infinity")
			} else {
				assert.True(t, row.WantedSince.Valid, "the row is wanted")
				assert.False(t, row.RefreshAfter.Time.After(time.Now()))
			}
		})
	}
}
