//go:build integration

package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

type grantsFixture struct {
	pool   *pgxpool.Pool
	grants *gateway.Grants
}

func startGrants(t *testing.T) grantsFixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	templates := catalog.New()

	add := func(name, environment string, tools map[string]v1alpha1.ToolOverride) {
		templates.Set(&v1alpha1.MCPServerTemplate{
			ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: name, Generation: 1, UID: types.UID("uid-" + name)},
			Spec: v1alpha1.MCPServerTemplateSpec{
				Scope:  "per-user",
				Access: v1alpha1.Access{Groups: []string{"dev"}, Environments: environment},
				Tools:  tools,
			},
		}, nil)
	}

	optIn := v1alpha1.ToolOverride{Environments: "opt-in"}

	add("optin-server", "opt-in", map[string]v1alpha1.ToolOverride{"query": optIn})
	add("default-server", "", nil)
	add("any-server", "any", map[string]v1alpha1.ToolOverride{"deploy": optIn, "read": {Environments: "any"}, "plain": {}})
	add("open-server", "any", nil)
	add("attested-server", "attested", nil)
	templates.Set(&v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: "secret-server", Generation: 1, UID: "uid-secret"},
		Spec:       v1alpha1.MCPServerTemplateSpec{Scope: "per-user", Access: v1alpha1.Access{Groups: []string{"ops"}}},
	}, nil)

	return grantsFixture{pool: pool, grants: gateway.NewGrants(pool, templates, io.Discard)}
}

var grantUser = auth.Identity{Principal: "erin", Groups: []string{"dev"}, Kind: auth.KindUser, Attestation: auth.AttestationUser}

func TestGrantCreationRules(t *testing.T) {
	fixture := startGrants(t)

	_, err := db.New(fixture.pool).CreateSession(t.Context(), db.CreateSessionParams{
		Principal: grantUser.Principal, SessionID: "open-session", Workspace: "w", Attestation: auth.AttestationUser, Groups: []string{},
	})
	require.NoError(t, err)

	ended, err := db.New(fixture.pool).CreateSession(t.Context(), db.CreateSessionParams{
		Principal: grantUser.Principal, SessionID: "ended-session", Workspace: "w", Attestation: auth.AttestationUser, Groups: []string{},
	})
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE id = $1", ended.ID)
	require.NoError(t, err)

	_, err = db.New(fixture.pool).CreateSession(t.Context(), db.CreateSessionParams{
		Principal: "frank", SessionID: "franks-session", Workspace: "w", Attestation: auth.AttestationUser, Groups: []string{},
	})
	require.NoError(t, err)

	tests := []struct {
		name        string
		request     gateway.GrantRequest
		wantRefusal string
	}{
		{name: "whole opt-in server", request: gateway.GrantRequest{Server: "optin-server", Lifetime: time.Hour}},
		{name: "server with the default environment", request: gateway.GrantRequest{Server: "default-server", Lifetime: time.Hour}},
		{name: "whole server with an opt-in tool", request: gateway.GrantRequest{Server: "any-server", Lifetime: time.Hour}},
		{name: "opt-in tool on an any server", request: gateway.GrantRequest{Server: "any-server", Tool: "deploy", Lifetime: time.Hour}},
		{name: "1 minute", request: gateway.GrantRequest{Server: "optin-server", Lifetime: time.Minute}},
		{name: "7 days", request: gateway.GrantRequest{Server: "optin-server", Lifetime: 7 * 24 * time.Hour}},
		{name: "open session", request: gateway.GrantRequest{Server: "optin-server", Session: "open-session", Lifetime: time.Hour}},
		{
			name: "tool on an opt-in server", wantRefusal: "can't have its own grant",
			request: gateway.GrantRequest{Server: "optin-server", Tool: "query", Lifetime: time.Hour},
		},
		{
			name: "tool with an any override", wantRefusal: "can't have its own grant",
			request: gateway.GrantRequest{Server: "any-server", Tool: "read", Lifetime: time.Hour},
		},
		{
			name: "tool without an override", wantRefusal: "can't have its own grant",
			request: gateway.GrantRequest{Server: "any-server", Tool: "plain", Lifetime: time.Hour},
		},
		{
			name: "unknown tool", wantRefusal: "can't have its own grant",
			request: gateway.GrantRequest{Server: "any-server", Tool: "missing", Lifetime: time.Hour},
		},
		{name: "server needing no grant", wantRefusal: "needs no grant", request: gateway.GrantRequest{Server: "open-server", Lifetime: time.Hour}},
		{name: "attested server", wantRefusal: "needs no grant", request: gateway.GrantRequest{Server: "attested-server", Lifetime: time.Hour}},
		{name: "no catalog access", wantRefusal: "isn't in your catalog", request: gateway.GrantRequest{Server: "secret-server", Lifetime: time.Hour}},
		{name: "unknown server", wantRefusal: "isn't in your catalog", request: gateway.GrantRequest{Server: "nowhere", Lifetime: time.Hour}},
		{
			name: "under 1 minute", wantRefusal: "1 minute to 7 days",
			request: gateway.GrantRequest{Server: "optin-server", Lifetime: time.Minute - time.Second},
		},
		{
			name: "over 7 days", wantRefusal: "1 minute to 7 days",
			request: gateway.GrantRequest{Server: "optin-server", Lifetime: 7*24*time.Hour + time.Second},
		},
		{
			name: "ended session", wantRefusal: "isn't one of your open sessions",
			request: gateway.GrantRequest{Server: "optin-server", Session: "ended-session", Lifetime: time.Hour},
		},
		{
			name: "another user's session", wantRefusal: "isn't one of your open sessions",
			request: gateway.GrantRequest{Server: "optin-server", Session: "franks-session", Lifetime: time.Hour},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grant, err := fixture.grants.Create(t.Context(), grantUser, test.request)

			if test.wantRefusal != "" {
				var refusal *gateway.GrantRefusal
				require.ErrorAs(t, err, &refusal)
				assert.Contains(t, refusal.Message, test.wantRefusal)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, grantUser.Principal, grant.Principal, "only for themselves")
			assert.Equal(t, test.request.Server, grant.TemplateName)
			assert.Equal(t, test.request.Tool, grant.Tool)
			assert.Equal(t, test.request.Session, grant.SessionID.String)
			assert.Equal(t, test.request.Session != "", grant.SessionID.Valid)
			assert.WithinDuration(t, grant.CreatedAt.Time.Add(test.request.Lifetime), grant.ExpiresAt.Time, time.Millisecond)
		})
	}
}

func TestGrantableServers(t *testing.T) {
	fixture := startGrants(t)

	assert.Equal(t, []gateway.GrantableServer{
		{Name: "any-server", WholeServer: true, Tools: []string{"deploy"}},
		{Name: "default-server", WholeServer: true},
		{Name: "optin-server", WholeServer: true},
	}, fixture.grants.Servers(grantUser))
}

func TestGrantLimit(t *testing.T) {
	fixture := startGrants(t)

	for index := range store.MaxLiveGrantsPerPrincipal {
		_, err := fixture.grants.Create(t.Context(), grantUser, gateway.GrantRequest{Server: "optin-server", Lifetime: time.Hour})
		require.NoError(t, err, "grant %d", index+1)
	}

	_, err := fixture.grants.Create(t.Context(), grantUser, gateway.GrantRequest{Server: "optin-server", Lifetime: time.Hour})

	var refusal *gateway.GrantRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "You have 100 live grants. Revoke some first.", refusal.Message)
}

func grantEvents(t *testing.T, pool *pgxpool.Pool, grantID string) []string {
	t.Helper()

	rows, err := pool.Query(t.Context(),
		"SELECT type || ' ' || principal || ' ' || server FROM audit_events WHERE details->>'grant' = $1 ORDER BY occurred_at, id", grantID)
	require.NoError(t, err)

	defer rows.Close()

	var events []string

	for rows.Next() {
		var event string
		require.NoError(t, rows.Scan(&event))
		events = append(events, event)
	}

	require.NoError(t, rows.Err())

	return events
}

func TestGrantCreateAndRevokeAreRecordedAndNotified(t *testing.T) {
	fixture := startGrants(t)

	listener := store.NewListener(fixture.pool, slog.New(slog.DiscardHandler))
	listenCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)

	go func() { _ = listener.Run(listenCtx) }()

	<-listener.Listening()

	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	grant, err := fixture.grants.Create(t.Context(), grantUser, gateway.GrantRequest{Server: "optin-server", Lifetime: time.Hour})
	require.NoError(t, err)

	requireGrantNotification(t, events, grant.ID)
	assert.Equal(t, []string{"grant.create erin optin-server"}, grantEvents(t, fixture.pool, grant.ID))

	_, err = fixture.grants.Revoke(t.Context(), "frank", grant.ID)
	require.ErrorIs(t, err, gateway.ErrGrantNotFound, "another user's grant is not found")

	_, err = fixture.grants.Get(t.Context(), "frank", grant.ID)
	require.ErrorIs(t, err, gateway.ErrGrantNotFound)

	revoked, err := fixture.grants.Revoke(t.Context(), grantUser.Principal, grant.ID)
	require.NoError(t, err)
	assert.True(t, revoked.RevokedAt.Valid)
	assert.Equal(t, grantUser.Principal, revoked.RevokedBy.String)

	requireGrantNotification(t, events, grant.ID)
	assert.Equal(t, []string{"grant.create erin optin-server", "grant.revoke erin optin-server"}, grantEvents(t, fixture.pool, grant.ID))

	_, err = fixture.grants.Revoke(t.Context(), grantUser.Principal, grant.ID)
	require.ErrorIs(t, err, gateway.ErrGrantNotFound, "a grant is revoked once")

	listed, err := fixture.grants.List(t.Context(), grantUser.Principal)
	require.NoError(t, err)
	require.Len(t, listed, 1, "a revoked grant stays on the list")

	var details map[string]any
	require.NoError(t, fixture.pool.QueryRow(t.Context(),
		"SELECT details FROM audit_events WHERE type = 'grant.create'").Scan(&details))
	assert.Equal(t, grant.ID, details["grant"])
	assert.Equal(t, templateNamespace, details["namespace"])
}

func requireGrantNotification(t *testing.T, events <-chan store.Event, id string) {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case event := <-events:
			if event.Kind == store.KindGrant && event.ID == id {
				return
			}
		case <-deadline:
			require.FailNow(t, "no grant notification", id)
		}
	}
}
