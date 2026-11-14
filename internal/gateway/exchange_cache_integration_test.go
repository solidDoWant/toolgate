//go:build integration

package gateway_test

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/resultcache"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// newCachedRefreshScene is a refresh scene whose gateway caches results and
// whose `echo` tool is cached and marked readOnlyHint, so a repeated call is
// a hit.
func newCachedRefreshScene(t *testing.T) *refreshScene {
	t.Helper()

	scene := newRefreshScene(t, func(options *gateway.MCPOptions) {
		options.ResultCache = resultcache.NewPostgres(options.Database, 1<<20, 64<<20)
	})

	scene.template.Spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Cache: &v1alpha1.ToolCache{}}}
	scene.catalog.Set(scene.template, nil)

	_, err := scene.pool.Exec(t.Context(), `UPDATE instances SET tools = $1 WHERE id = $2`, readOnlyTools(t, "echo"), scene.instance.ID)
	require.NoError(t, err)

	return scene
}

// requireHit fails unless result is a successful cache hit of the echo call.
func requireHit(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()

	require.False(t, result.IsError, "%s", resultText(result))
	assert.Equal(t, "hi", resultText(result))

	_, found := cacheAge(result)
	require.True(t, found, "the call is served from the cache")
}

// patchedTokens records the token values the leader patches into instance
// Secrets.
type patchedTokens struct {
	mu     sync.Mutex
	bodies []string
}

func (p *patchedTokens) client() client.Client {
	return clientfake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, object client.Object, patch client.Patch, _ ...client.PatchOption) error {
			body, err := patch.Data(object)
			if err != nil {
				return err
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			p.bodies = append(p.bodies, string(body))

			return nil
		},
	}).Build()
}

func (p *patchedTokens) contains(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	encoded := base64.StdEncoding.EncodeToString([]byte(token))

	for _, body := range p.bodies {
		if strings.Contains(body, encoded) {
			return true
		}
	}

	return false
}

func TestCacheHitRefreshesExchangedToken(t *testing.T) {
	scene := newCachedRefreshScene(t)

	first := scene.echo(t)
	require.False(t, first.IsError, "%s", resultText(first))
	_, found := cacheAge(first)
	require.False(t, found, "the first call is forwarded")

	requireHit(t, scene.echo(t))
	assert.Equal(t, int64(1), scene.endpoint.requests.Load(), "a hit before the half-life exchanges nothing")

	oldExpiry := scene.exchangeRow(t).ExpiresAt.Time

	scene.makeRefreshDue(t)
	requireHit(t, scene.echo(t))

	require.Eventually(t, func() bool { return len(scene.refreshEvents(t)) == 1 },
		30*time.Second, 10*time.Millisecond, "the hit's refresh is recorded")
	assert.Equal(t, "success", scene.refreshEvents(t)[0]["outcome"])

	delivery := scene.awaitDelivery(t, scene.instance.ID)
	assert.Equal(t, "exchanged-for-alice-2", string(delivery.Token))

	// The leader patches the new token into the running instance's Secret.
	patched := &patchedTokens{}
	refresher := &runtime.ExchangeRefresher{Pool: scene.pool, Client: patched.client(), Tokens: scene.tokens}

	live, err := db.New(scene.pool).GetInstance(t.Context(), scene.instance.ID)
	require.NoError(t, err)
	require.NoError(t, refresher.Step(t.Context(), live))

	assert.True(t, patched.contains("exchanged-for-alice-2"), "the Secret gets the refreshed token")
	assert.True(t, scene.exchangeRow(t).ExpiresAt.Time.After(oldExpiry), "the token in the Secret now expires later")

	// At the old expiry, no token of the instance has expired, so the stop
	// pass leaves it running.
	_, err = db.New(scene.pool).FirstExpiredInstanceExchange(t.Context(), db.FirstExpiredInstanceExchangeParams{
		InstanceID: scene.instance.ID, Now: pgtype.Timestamptz{Time: oldExpiry, Valid: true},
	})
	require.ErrorIs(t, err, pgx.ErrNoRows)

	live, err = db.New(scene.pool).GetInstance(t.Context(), scene.instance.ID)
	require.NoError(t, err)
	assert.Equal(t, "ready", live.Status, "the instance is not replaced")
	assert.Equal(t, scene.instance.Generation, live.Generation)
	assert.Len(t, scene.instancesOf(t, scene.template), 1)

	requireHit(t, scene.echo(t))
}

func TestRefusedExchangeEndsCacheHits(t *testing.T) {
	scene := newCachedRefreshScene(t)

	require.False(t, scene.echo(t).IsError)
	requireHit(t, scene.echo(t))

	scene.endpoint.refuse.Store("invalid_grant")
	scene.makeRefreshDue(t)

	requireHit(t, scene.echo(t))

	require.Eventually(t, func() bool { return scene.exchangeRow(t).LastError.Valid },
		30*time.Second, 10*time.Millisecond, "the hit's refused refresh is recorded")

	events := scene.refreshEvents(t)
	require.Len(t, events, 1)
	assert.Equal(t, "failure", events[0]["outcome"])

	// Hits go on while the token in the Secret is valid.
	requireHit(t, scene.echo(t))

	// The token expires: the stop pass stops the instance, and the next call
	// needs a new instance, whose exchange the IdP refuses.
	scene.expire(t)

	results := scene.callInBackground(t)
	next := scene.newInstance(t)

	require.Eventually(t, func() bool {
		rows, err := db.New(scene.pool).ListInstanceExchanges(t.Context(), next.ID)

		return err == nil && len(rows) == 1 && rows[0].LastError.Valid
	}, 30*time.Second, 10*time.Millisecond, "the refused start is recorded")

	scene.instance = next
	next = scene.step(t, next.ID)
	require.Equal(t, "failed", next.Status)

	select {
	case result := <-results:
		require.True(t, result.IsError, "the call is refused instead of hit")
		assert.Contains(t, resultText(result), "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`.")

		_, found := cacheAge(result)
		assert.False(t, found)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the call did not return")
	}
}
