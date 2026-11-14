//go:build integration

package gateway_test

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// refreshScene is an exchange fixture whose `temporal-mcp` server is ready
// for alice with its first token in the Secret.
type refreshScene struct {
	*exchangeFixture

	template *v1alpha1.MCPServerTemplate
	client   user
	session  *mcp.ClientSession
	instance db.Instance
	logs     *lockedBuffer
}

func newRefreshScene(t *testing.T, extra ...func(*gateway.MCPOptions)) *refreshScene {
	t.Helper()

	logs := &lockedBuffer{}
	fixture := newExchangeStartFixture(t, append([]func(*gateway.MCPOptions){func(options *gateway.MCPOptions) {
		options.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}}, extra...)...)
	template := fixture.addTokenTemplate("temporal-mcp")
	fixture.startUpstream(t, template.Name)

	client := alice()
	client.token = fixture.provider.MintAccessToken(t, client.claims)

	scene := &refreshScene{exchangeFixture: fixture, template: template, client: client, logs: logs}
	scene.session = fixture.connect(t, client)

	results := startEnable(t, scene.session, template.Name)
	instance := fixture.awaitInstance(t, template, "user:alice")
	fixture.awaitDelivery(t, instance.ID)

	instance = fixture.step(t, instance.ID)
	require.Equal(t, "starting", instance.Status, instance.Reason)
	fixture.markReady(t, instance.ID, "echo")
	require.Empty(t, awaitEnable(t, results).err)

	scene.instance = instance

	return scene
}

func (s *refreshScene) echo(t *testing.T) *mcp.CallToolResult {
	t.Helper()

	return callTool(t, s.session, s.template.Name+"__echo", map[string]any{"text": "hi"})
}

// makeRefreshDue moves the half-life of the token into the past, as time
// passing would.
func (s *refreshScene) makeRefreshDue(t *testing.T) {
	t.Helper()

	_, err := s.pool.Exec(t.Context(),
		"UPDATE instance_exchanges SET refresh_after = now() - interval '1 second' WHERE instance_id = $1", s.instance.ID)
	require.NoError(t, err)
}

func (s *refreshScene) exchangeRow(t *testing.T) db.InstanceExchange {
	t.Helper()

	rows, err := db.New(s.pool).ListInstanceExchanges(t.Context(), s.instance.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	return rows[0]
}

func (s *refreshScene) refreshEvents(t *testing.T) []map[string]any {
	t.Helper()

	var refreshes []map[string]any

	for _, event := range s.exchangeEvents(t, s.template.Name) {
		if event["kind"] == "refresh" {
			refreshes = append(refreshes, event)
		}
	}

	return refreshes
}

// expire stops the instance as the stop pass does for an expired token.
func (s *refreshScene) expire(t *testing.T) {
	t.Helper()

	rows, err := db.New(s.pool).SetInstanceDesiredStoppedExpired(t.Context(), s.instance.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	_, err = s.pool.Exec(t.Context(), "UPDATE instances SET status = 'stopped' WHERE id = $1", s.instance.ID)
	require.NoError(t, err)
}

// startedAfterExpiry makes a call that starts the new instance, and returns
// the channel of its result.
func (s *refreshScene) callInBackground(t *testing.T) <-chan *mcp.CallToolResult {
	t.Helper()

	results := make(chan *mcp.CallToolResult, 1)
	ctx := t.Context()

	go func() {
		result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: s.template.Name + "__echo", Arguments: map[string]any{"text": "hi"}})
		if err != nil {
			result = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
		}

		results <- result
	}()

	return results
}

func (s *refreshScene) newInstance(t *testing.T) db.Instance {
	t.Helper()

	var found db.Instance

	require.Eventually(t, func() bool {
		for _, instance := range s.instancesOf(t, s.template) {
			if instance.ID != s.instance.ID {
				found = instance

				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond, "a new instance is inserted")

	return found
}

func TestRefreshAtHalfLifeKeepsSessionAndInstance(t *testing.T) {
	scene := newRefreshScene(t)

	// Before the half-life a call exchanges nothing.
	require.Equal(t, "hi", resultText(scene.echo(t)))
	assert.Equal(t, int64(1), scene.endpoint.requests.Load())

	scene.makeRefreshDue(t)
	require.Equal(t, "hi", resultText(scene.echo(t)), "the call is forwarded while the refresh runs")

	require.Eventually(t, func() bool { return len(scene.refreshEvents(t)) == 1 },
		30*time.Second, 10*time.Millisecond, "the refresh is recorded")

	delivery := scene.awaitDelivery(t, scene.instance.ID)
	assert.Equal(t, "exchanged-for-alice-2", string(delivery.Token))
	assert.Equal(t, "success", scene.refreshEvents(t)[0]["outcome"])

	live, err := db.New(scene.pool).GetInstance(t.Context(), scene.instance.ID)
	require.NoError(t, err)
	assert.Equal(t, "ready", live.Status, "the instance is not replaced")
	assert.Equal(t, scene.instance.Generation, live.Generation)
	assert.Len(t, scene.instancesOf(t, scene.template), 1)

	assert.Equal(t, "hi", resultText(scene.echo(t)), "the session still has the server enabled")
}

func TestOneRefreshPerHalfLife(t *testing.T) {
	scene := newRefreshScene(t)
	scene.makeRefreshDue(t)

	var wait sync.WaitGroup

	for range 6 {
		wait.Add(1)

		go func() {
			defer wait.Done()

			assert.Equal(t, "hi", resultText(scene.echo(t)))
		}()
	}

	wait.Wait()

	require.Eventually(t, func() bool { return len(scene.refreshEvents(t)) == 1 },
		30*time.Second, 10*time.Millisecond, "one refresh runs")

	// The claim is held until the leader applies the token, so later calls
	// in the same half-life exchange nothing.
	for range 3 {
		require.Equal(t, "hi", resultText(scene.echo(t)))
	}

	assert.Equal(t, int64(2), scene.endpoint.requests.Load(), "the start and one refresh")
	assert.Len(t, scene.refreshEvents(t), 1)
}

func TestFailedRefreshKeepsTokenUntilExpiry(t *testing.T) {
	scene := newRefreshScene(t)
	scene.endpoint.refuse.Store("invalid_grant")
	scene.makeRefreshDue(t)

	require.Equal(t, "hi", resultText(scene.echo(t)), "a failed refresh never fails the call")

	require.Eventually(t, func() bool { return scene.exchangeRow(t).LastError.Valid },
		30*time.Second, 10*time.Millisecond, "the failure is recorded")

	row := scene.exchangeRow(t)
	assert.Equal(t, "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`.", row.LastError.String)
	assert.False(t, row.ClaimedAt.Valid, "a failure gives up the claim")

	var retryIn time.Duration

	require.NoError(t, scene.pool.QueryRow(t.Context(),
		"SELECT refresh_after - now() FROM instance_exchanges WHERE instance_id = $1", scene.instance.ID).Scan(&retryIn))
	assert.Greater(t, retryIn, 50*time.Second, "the next try is in about 60 seconds")
	assert.LessOrEqual(t, retryIn, 60*time.Second)

	live, err := db.New(scene.pool).GetInstance(t.Context(), scene.instance.ID)
	require.NoError(t, err)
	assert.Equal(t, "ready", live.Status)
	assert.False(t, live.FailedAt.Valid)

	events := scene.refreshEvents(t)
	require.Len(t, events, 1)
	assert.Equal(t, "failure", events[0]["outcome"])

	// The retry never goes past the expiry of the token in the Secret.
	_, err = scene.pool.Exec(t.Context(),
		"UPDATE instance_exchanges SET expires_at = now() + interval '20 seconds', refresh_after = now() - interval '1 second' WHERE instance_id = $1", scene.instance.ID)
	require.NoError(t, err)

	require.Equal(t, "hi", resultText(scene.echo(t)))

	require.Eventually(t, func() bool { return len(scene.refreshEvents(t)) == 2 },
		30*time.Second, 10*time.Millisecond, "the second failure is recorded")

	require.NoError(t, scene.pool.QueryRow(t.Context(),
		"SELECT refresh_after - now() FROM instance_exchanges WHERE instance_id = $1", scene.instance.ID).Scan(&retryIn))
	assert.LessOrEqual(t, retryIn, 20*time.Second, "the retry waits at most until the token expires")
}

func TestCallAfterExpiryStopStartsNewInstance(t *testing.T) {
	scene := newRefreshScene(t)
	scene.expire(t)

	results := scene.callInBackground(t)
	next := scene.newInstance(t)

	assert.Equal(t, "pending", next.Status)
	assert.True(t, next.UsesExchange)

	delivery := scene.awaitDelivery(t, next.ID)
	assert.Equal(t, "exchanged-for-alice-2", string(delivery.Token), "the new instance exchanges at once")

	scene.instance = next
	next = scene.step(t, next.ID)
	require.Equal(t, "starting", next.Status, next.Reason)
	scene.markReady(t, next.ID, "echo")

	select {
	case result := <-results:
		require.False(t, result.IsError, "%s", resultText(result))
		assert.Equal(t, "hi", resultText(result))
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the call did not return")
	}
}

func TestRefusedRefreshThenRefusedStart(t *testing.T) {
	scene := newRefreshScene(t)
	scene.endpoint.refuse.Store("invalid_grant")

	scene.makeRefreshDue(t)
	require.Equal(t, "hi", resultText(scene.echo(t)))

	require.Eventually(t, func() bool { return scene.exchangeRow(t).LastError.Valid },
		30*time.Second, 10*time.Millisecond, "the refused refresh is recorded")

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
		require.True(t, result.IsError)
		assert.Contains(t, resultText(result), "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`.")
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the call did not return")
	}

	assert.Len(t, scene.instancesOf(t, scene.template), 1, "no second start is inserted while the newest failed")
}

func TestNoTokenInDatabaseLogsOrConditions(t *testing.T) {
	scene := newRefreshScene(t)
	scene.makeRefreshDue(t)
	require.Equal(t, "hi", resultText(scene.echo(t)))

	require.Eventually(t, func() bool { return len(scene.refreshEvents(t)) == 1 },
		30*time.Second, 10*time.Millisecond)
	scene.awaitDelivery(t, scene.instance.ID)

	secrets := []string{scene.client.token, "exchanged-for-alice-", "client-secret"}

	tables, err := scene.pool.Query(t.Context(), "SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
	require.NoError(t, err)

	names, err := pgx.CollectRows(tables, pgx.RowTo[string])
	require.NoError(t, err)
	require.NotEmpty(t, names)

	for _, name := range names {
		var dump string

		require.NoError(t, scene.pool.QueryRow(t.Context(),
			fmt.Sprintf("SELECT coalesce(string_agg(row_to_json(t)::text, E'\\n'), '') FROM %s t", pgx.Identifier{name}.Sanitize())).Scan(&dump))

		for _, secret := range secrets {
			assert.NotContains(t, dump, secret, "table %s holds a secret", name)
		}
	}

	logged := scene.logs.String()
	for _, secret := range secrets {
		assert.False(t, strings.Contains(logged, secret), "the log holds a secret")
	}

	status, found := scene.catalog.Status(types.NamespacedName{Namespace: scene.template.Namespace, Name: scene.template.Name})
	require.True(t, found)

	for _, secret := range secrets {
		assert.NotContains(t, fmt.Sprintf("%+v", status), secret, "a template condition holds a secret")
	}
}
