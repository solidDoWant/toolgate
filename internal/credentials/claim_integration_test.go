//go:build integration

package credentials_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const owner = "user:alice"

type fixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	counter atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	return &fixture{pool: pool, queries: db.New(pool)}
}

// instance inserts a live starting instance that uses exchanges, with one
// due, unclaimed row for each named exchange.
func (f *fixture) instance(t *testing.T, exchanges ...string) string {
	t.Helper()

	id := fmt.Sprintf("%032x", f.counter.Add(1))
	_, err := f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: "default", TemplateName: "temporal", TemplateUid: "uid", Generation: 1,
		Scope: "per-user", ScopeKey: id, OwnerPrincipal: owner, Namespace: "tg-" + id,
	})
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), "UPDATE instances SET uses_exchange = true, status = 'starting' WHERE id = $1", id)
	require.NoError(t, err)

	for _, exchange := range exchanges {
		require.NoError(t, f.queries.InsertInstanceExchange(t.Context(), db.InsertInstanceExchangeParams{
			InstanceID: id, Exchange: exchange,
		}))
	}

	return id
}

// claimedAgo sets the claim of a row to have been taken ago before the
// database's clock.
func (f *fixture) claimedAgo(t *testing.T, id, exchange string, ago time.Duration) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(),
		"UPDATE instance_exchanges SET claimed_at = now() - make_interval(secs => $3) WHERE instance_id = $1 AND exchange = $2",
		id, exchange, ago.Seconds())
	require.NoError(t, err)
}

func userCaller(expiry time.Time) credentials.Caller {
	return credentials.Caller{Principal: owner, Kind: auth.KindUser, TokenExpiry: expiry}
}

func TestClaimSkipsExpiredSubjectToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	now := time.Now()
	claimer := &credentials.Claimer{Now: func() time.Time { return now }}
	timeouts := map[string]time.Duration{"temporal": 10 * time.Second}

	tests := map[string]struct {
		expiry time.Time
		want   []string
	}{
		"expired a second ago":   {expiry: now.Add(-time.Second), want: nil},
		"expires now":            {expiry: now, want: nil},
		"expires in one second":  {expiry: now.Add(time.Second), want: []string{"temporal"}},
		"zero expiry (no token)": {expiry: time.Time{}, want: nil},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			id := f.instance(t, "temporal")

			got, err := claimer.Claim(t.Context(), f.queries, id, userCaller(test.expiry), timeouts)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestExpiredClaimCanBeTakenAgain(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	claimer := &credentials.Claimer{}
	timeouts := map[string]time.Duration{"temporal": 10 * time.Second}
	caller := userCaller(time.Now().Add(time.Hour))
	id := f.instance(t, "temporal")

	got, err := claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Equal(t, []string{"temporal"}, got)

	got, err = claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Empty(t, got, "a held claim is not taken again")

	f.claimedAgo(t, id, "temporal", 35*time.Second)

	got, err = claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Empty(t, got, "the claim lasts the exchange timeout plus 30 s")

	f.claimedAgo(t, id, "temporal", 41*time.Second)

	got, err = claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Equal(t, []string{"temporal"}, got)
}

func TestClaimUsesEachExchangesTimeout(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	claimer := &credentials.Claimer{}
	caller := userCaller(time.Now().Add(time.Hour))
	timeouts := map[string]time.Duration{"short": 10 * time.Second, "long": 60 * time.Second}
	id := f.instance(t, "short", "long")

	// 45 s: past short's 40 s, before long's 90 s.
	f.claimedAgo(t, id, "short", 45*time.Second)
	f.claimedAgo(t, id, "long", 45*time.Second)

	got, err := claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Equal(t, []string{"short"}, got)

	// 95 s: past long's 90 s too.
	f.claimedAgo(t, id, "long", 95*time.Second)

	got, err = claimer.Claim(t.Context(), f.queries, id, caller, timeouts)
	require.NoError(t, err)
	assert.Equal(t, []string{"long"}, got)
}

func TestClaimIgnoresExchangeNotConfigured(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	claimer := &credentials.Claimer{}
	id := f.instance(t, "temporal", "removed")

	got, err := claimer.Claim(t.Context(), f.queries, id, userCaller(time.Now().Add(time.Hour)),
		map[string]time.Duration{"temporal": 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, []string{"temporal"}, got)

	rows, err := f.queries.ListInstanceExchanges(t.Context(), id)
	require.NoError(t, err)

	for _, row := range rows {
		assert.Equal(t, row.Exchange == "temporal", row.ClaimedAt.Valid, row.Exchange)
	}
}

func TestClaimNeedsLiveOwnedInstanceAndUser(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	claimer := &credentials.Claimer{}
	timeouts := map[string]time.Duration{"temporal": 10 * time.Second}
	valid := userCaller(time.Now().Add(time.Hour))

	tests := map[string]struct {
		prepare func(t *testing.T, id string)
		caller  credentials.Caller
	}{
		"other principal":  {caller: credentials.Caller{Principal: "user:bob", Kind: auth.KindUser, TokenExpiry: valid.TokenExpiry}},
		"service account":  {caller: credentials.Caller{Principal: owner, Kind: auth.KindServiceAccount, TokenExpiry: valid.TokenExpiry}},
		"instance stopped": {caller: valid, prepare: func(t *testing.T, id string) { f.setColumn(t, id, "status = 'stopped'") }},
		"desired stopped":  {caller: valid, prepare: func(t *testing.T, id string) { f.setColumn(t, id, "desired = 'stopped'") }},
		"no exchange use":  {caller: valid, prepare: func(t *testing.T, id string) { f.setColumn(t, id, "uses_exchange = false") }},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			id := f.instance(t, "temporal")
			if test.prepare != nil {
				test.prepare(t, id)
			}

			got, err := claimer.Claim(t.Context(), f.queries, id, test.caller, timeouts)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

func (f *fixture) setColumn(t *testing.T, id, set string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET "+set+" WHERE id = $1", id)
	require.NoError(t, err)
}
