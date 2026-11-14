//go:build integration

package gateway_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// keyAPI is the workspace key API over a fixture's database, with workspace
// keys on or off.
type keyAPI struct {
	*workspaceAPI
}

func newKeyAPI(t *testing.T, fixture *mcpFixture, keysOn bool) *keyAPI {
	t.Helper()

	options := attestedUsers()
	options.IssuerURL, options.Audiences = fixture.provider.Issuer, []string{fixture.provider.Audience}

	authenticator, err := auth.New(t.Context(), *options)
	require.NoError(t, err)

	workspaces := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0)
	workspaces.SetWorkspaceKeys(keysOn, "")

	mux := http.NewServeMux()
	(&workspacesapi.Handler{
		Workspaces: workspaces, Keys: workspaces, Enabled: true,
		ApprovalURL: func(id string) string { return approvalPrefix + id },
	}).Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &keyAPI{&workspaceAPI{fixture: fixture, server: server, workspaces: workspaces}}
}

// keyUser is a user whose token has preferred_username set to its subject.
func keyUser(subject string) user {
	return user{
		claims:  testutil.TokenClaims{Subject: subject, Extra: map[string]any{"preferred_username": subject}},
		session: "session-1",
	}
}

// keyIdentity is keyUser's identity, for the calls the approval page makes.
func keyIdentity(subject string) auth.Identity {
	return auth.Identity{
		Principal: subject, Username: subject, Workspace: "claimed/default",
		Kind: auth.KindUser, Attestation: auth.AttestationUser,
	}
}

func newPublicKey(t *testing.T) string {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	return base64.RawURLEncoding.EncodeToString(public)
}

func (a *keyAPI) register(t *testing.T, client user, name, publicKey string) (int, string) {
	t.Helper()

	return a.do(t, &client, http.MethodPost, workspacesapi.WorkspaceKeysPath, workspacesapi.KeyRegistration{Name: name, PublicKey: publicKey})
}

// requestKey registers a key and returns its pending request.
func (a *keyAPI) requestKey(t *testing.T, client user, name, publicKey string) workspacesapi.KeyRequested {
	t.Helper()

	status, body := a.register(t, client, name, publicKey)
	require.Equal(t, http.StatusAccepted, status, body)

	var requested workspacesapi.KeyRequested
	require.NoError(t, json.Unmarshal([]byte(body), &requested))
	require.Regexp(t, `^[0-9a-f]{32}$`, requested.Request)

	return requested
}

func (a *keyAPI) approveKey(t *testing.T, principal, id string) error {
	t.Helper()

	return pgx.BeginFunc(t.Context(), a.fixture.pool, func(tx pgx.Tx) error {
		return a.workspaces.ApproveKeyRequest(t.Context(), tx, keyIdentity(principal), id)
	})
}

// activateKey registers and approves a key.
func (a *keyAPI) activateKey(t *testing.T, principal, name, publicKey string) {
	t.Helper()

	requested := a.requestKey(t, keyUser(principal), name, publicKey)
	require.NoError(t, a.approveKey(t, principal, requested.Request))
}

func (a *keyAPI) count(t *testing.T, query string, arguments ...any) int {
	t.Helper()

	var count int

	require.NoError(t, a.fixture.pool.QueryRow(t.Context(), query, arguments...).Scan(&count))

	return count
}

func (a *keyAPI) keyRequests(t *testing.T, principal string) int {
	t.Helper()

	return a.count(t, `SELECT count(*) FROM access_requests WHERE principal = $1 AND kind = 'key'`, principal)
}

func (a *keyAPI) activeKeys(t *testing.T, principal string) int {
	t.Helper()

	return a.count(t, `SELECT count(*) FROM workspace_keys WHERE principal = $1 AND status = 'active'`, principal)
}

func (a *keyAPI) events(t *testing.T, principal, eventType string) int {
	t.Helper()

	return a.count(t, `SELECT count(*) FROM audit_events WHERE principal = $1 AND type = $2`, principal, eventType)
}

func (a *keyAPI) requestStatusOf(t *testing.T, id string) string {
	t.Helper()

	var status string

	require.NoError(t, a.fixture.pool.QueryRow(t.Context(), `SELECT status FROM access_requests WHERE id = $1`, id).Scan(&status))

	return status
}

// insertActiveKeys inserts count active keys of principal with made-up key
// IDs, as earlier approvals would have.
func (a *keyAPI) insertActiveKeys(t *testing.T, principal string, count int) {
	t.Helper()

	for index := range count {
		keyID := fmt.Sprintf("%s%04d", strings.Repeat("k", 39), index)
		_, err := a.fixture.pool.Exec(t.Context(), `INSERT INTO workspace_keys
			(principal, username, name, key_id, public_key, status, request_id, registered_at, approved_at)
			VALUES ($1, $1, $2, $3, decode(repeat('00', 32), 'hex'), 'active', 'seed', now(), now())`,
			principal, fmt.Sprintf("seed-%d", index), keyID[:43])
		require.NoError(t, err)
	}
}

func TestWorkspaceKeyRegistration(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	t.Run("user token alone makes no usable key", func(t *testing.T) {
		key := newWorkspaceKey(t)
		api.requestKey(t, keyUser("reg-alone"), "dev", key.publicKey())

		client := keyUser("reg-alone")
		client.session = "alone"
		fixture.startSession(t, fixture.server.URL, withKey(t, client, key))

		assert.Equal(t, "attestation key not approved", refusals.last())
		fixture.requireSession(t, client, "claimed/default", auth.AttestationUser)
		assert.Zero(t, api.count(t, `SELECT count(*) FROM workspace_keys WHERE principal = 'reg-alone'`))
	})

	t.Run("same key twice returns the pending request", func(t *testing.T) {
		key := newPublicKey(t)
		first := api.requestKey(t, keyUser("reg-twice"), "dev", key)
		assert.Equal(t, approvalPrefix+first.Request, first.URL)
		assert.Equal(t, "registered/reg-twice/dev", first.Workspace)
		assert.Nil(t, first.Replaces)

		second := api.requestKey(t, keyUser("reg-twice"), "dev", key)
		assert.Equal(t, first, second)
		assert.Equal(t, 1, api.keyRequests(t, "reg-twice"))
		assert.Equal(t, 1, api.events(t, "reg-twice", gateway.TypeWorkspaceKeyRegister))
		assert.Equal(t, 1, api.events(t, "reg-twice", gateway.TypeAccessRequest))
	})

	t.Run("active key ID is refused", func(t *testing.T) {
		key := newPublicKey(t)
		api.activateKey(t, "reg-taken", "dev", key)

		status, body := api.register(t, keyUser("reg-taken"), "dev", key)
		assert.Equal(t, http.StatusOK, status, "the same name and key is already active")

		var active workspacesapi.KeyActive
		require.NoError(t, json.Unmarshal([]byte(body), &active))
		assert.Equal(t, workspacesapi.KeyActive{
			Status: "active", Name: "dev", Workspace: "registered/reg-taken/dev", Fingerprint: active.Fingerprint,
		}, active)
		assert.True(t, strings.HasPrefix(active.Fingerprint, "SHA256:"))

		for _, attempt := range []struct{ subject, name string }{{"reg-taken", "other"}, {"reg-thief", "dev"}} {
			status, body := api.register(t, keyUser(attempt.subject), attempt.name, key)
			assert.Equal(t, http.StatusConflict, status)
			assert.Equal(t, "This key is already registered. Make a new key with a new --key-file.", body)
		}

		assert.Equal(t, 1, api.keyRequests(t, "reg-taken"))
		assert.Zero(t, api.keyRequests(t, "reg-thief"))
	})

	t.Run("a non-user identity gets 403", func(t *testing.T) {
		_, err := api.workspaces.RegisterKey(t.Context(), auth.Identity{
			Principal: "system:serviceaccount:ci:runner", Username: "runner", Kind: auth.KindServiceAccount,
			Workspace: "k8s/ci/Pod/runner", Attestation: auth.AttestationAttested,
		}, "dev", newPublicKey(t))

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusForbidden, refusal.Status)
		assert.Equal(t, "Only users can register workspace keys.", refusal.Message)
		assert.Zero(t, api.keyRequests(t, "system:serviceaccount:ci:runner"))
	})

	t.Run("the pending limit", func(t *testing.T) {
		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
			SELECT md5(n::text), 'reg-limit', 'change', 'claimed/default', false, '{"action":"defaults"}', now() + interval '1 hour'
			FROM generate_series(1, $1::int - 1) AS n`, gateway.MaxPendingAccessRequestsPerPrincipal)
		require.NoError(t, err)

		api.requestKey(t, keyUser("reg-limit"), "last", newPublicKey(t))

		status, body := api.register(t, keyUser("reg-limit"), "over", newPublicKey(t))
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "You already have 100 pending access requests. Approve or deny some first.", body)
		assert.Equal(t, 1, api.keyRequests(t, "reg-limit"))
	})

	t.Run("the input checks answer in order", func(t *testing.T) {
		noUsername := user{claims: testutil.TokenClaims{Subject: "reg-checks"}, session: "session-1"}

		for _, check := range []struct {
			name       string
			client     user
			keyName    string
			publicKey  string
			wantStatus int
			wantBody   string
		}{
			{"bad name", keyUser("reg-checks"), "a/b", "x", http.StatusBadRequest, "A key name has 1 to 63 letters, digits, `.`, `_` or `-`."},
			{"name of 64", keyUser("reg-checks"), strings.Repeat("a", 64), newPublicKey(t), http.StatusBadRequest,
				"A key name has 1 to 63 letters, digits, `.`, `_` or `-`."},
			{"short key", keyUser("reg-checks"), "dev", base64.RawURLEncoding.EncodeToString(make([]byte, 31)), http.StatusBadRequest,
				"publicKey must be an Ed25519 public key of 32 bytes, base64url without padding."},
			{"no user name", noUsername, "dev", newPublicKey(t), http.StatusBadRequest,
				"Your sign-in has no user name that fits in a workspace ID. toolgate needs the preferred_username claim, " +
					"with 1 to 63 letters, digits, `.`, `_` or `-`."},
		} {
			status, body := api.register(t, check.client, check.keyName, check.publicKey)
			assert.Equal(t, check.wantStatus, status, check.name)
			assert.Equal(t, check.wantBody, body, check.name)
		}

		api.requestKey(t, keyUser("reg-checks"), strings.Repeat("a", 63), newPublicKey(t))

		status, _ := api.do(t, ptr(keyUser("reg-checks")), http.MethodPost, workspacesapi.WorkspaceKeysPath,
			`{"name":"dev","publicKey":"`+strings.Repeat("a", workspacesapi.MaxKeyRegistrationBytes)+`"}`)
		assert.Equal(t, http.StatusRequestEntityTooLarge, status)
	})

	t.Run("registration while keys are off is refused", func(t *testing.T) {
		off := newKeyAPI(t, fixture, false)

		status, body := off.register(t, keyUser("reg-off"), "dev", newPublicKey(t))
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "Workspace keys are off on this gateway.", body)
		assert.Zero(t, api.keyRequests(t, "reg-off"))
	})

	t.Run("every route needs a user", func(t *testing.T) {
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, workspacesapi.WorkspaceKeysPath},
			{http.MethodGet, workspacesapi.WorkspaceKeysPath},
			{http.MethodDelete, workspacesapi.WorkspaceKeysPath + "/dev"},
		} {
			status, _ := api.do(t, nil, route.method, route.path, `{}`)
			assert.Equal(t, http.StatusUnauthorized, status, "%s %s", route.method, route.path)
		}
	})
}

func TestWorkspaceKeyApproval(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	t.Run("approved key attests on another replica", func(t *testing.T) {
		key := newWorkspaceKey(t)
		requested := api.requestKey(t, keyUser("app-replica"), "dev", key.publicKey())

		client := keyUser("app-replica")
		client.session = "replica"
		other := fixture.replica(t)

		fixture.startSession(t, other.URL, withKey(t, client, key))
		fixture.requireSession(t, client, "claimed/default", auth.AttestationUser)

		require.NoError(t, api.approveKey(t, "app-replica", requested.Request))

		fresh := client
		fresh.session = "replica-after"
		fixture.startSession(t, other.URL, withKey(t, fresh, key))
		fixture.requireSession(t, fresh, "registered/app-replica/dev", auth.AttestationAttested)
	})

	t.Run("replace keeps the workspace", func(t *testing.T) {
		oldKey, newKey := newWorkspaceKey(t), newWorkspaceKey(t)
		api.activateKey(t, "app-keeps", "dev", oldKey.publicKey())

		client := keyUser("app-keeps")
		client.session = "keeps"
		mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, oldKey))
		require.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, oldKey), mcpSession))

		replacement := api.requestKey(t, keyUser("app-keeps"), "dev", newKey.publicKey())
		require.NoError(t, api.approveKey(t, "app-keeps", replacement.Request))

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, oldKey), mcpSession))
		assert.Equal(t, "attestation key revoked", refusals.last())

		assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, newKey), mcpSession),
			"the new key gives the same workspace")

		fresh := client
		fresh.session = "keeps-new"
		fixture.startSession(t, fixture.server.URL, withKey(t, fresh, newKey))
		fixture.requireSession(t, fresh, "registered/app-keeps/dev", auth.AttestationAttested)
	})

	t.Run("approval makes the key active with the request's time", func(t *testing.T) {
		requested := api.requestKey(t, keyUser("app-row"), "dev", newPublicKey(t))
		require.NoError(t, api.approveKey(t, "app-row", requested.Request))

		assert.Equal(t, 1, api.count(t, `SELECT count(*) FROM workspace_keys k JOIN access_requests r ON r.id = k.request_id
			WHERE k.principal = 'app-row' AND k.status = 'active' AND k.username = 'app-row' AND k.registered_at = r.created_at
			  AND k.ended_at IS NULL AND r.status = 'approved'`))
		assert.Equal(t, 1, api.events(t, "app-row", gateway.TypeWorkspaceKeyApprove))
		assert.Equal(t, 1, api.events(t, "app-row", gateway.TypeAccessApprove))
	})

	t.Run("replace ends the old key in the same transaction", func(t *testing.T) {
		api.activateKey(t, "app-replace", "dev", newPublicKey(t))

		requested := api.requestKey(t, keyUser("app-replace"), "dev", newPublicKey(t))
		require.NotNil(t, requested.Replaces)

		var oldKeyID string

		require.NoError(t, fixture.pool.QueryRow(t.Context(),
			`SELECT key_id FROM workspace_keys WHERE principal = 'app-replace' AND status = 'active'`).Scan(&oldKeyID))
		assert.Equal(t, "SHA256:"+oldKeyID, *requested.Replaces)

		require.NoError(t, api.approveKey(t, "app-replace", requested.Request))

		assert.Equal(t, 1, api.activeKeys(t, "app-replace"))
		assert.Equal(t, 1, api.count(t, `SELECT count(*) FROM workspace_keys
			WHERE principal = 'app-replace' AND key_id = $1 AND status = 'replaced' AND ended_at IS NOT NULL`, oldKeyID))
		assert.Equal(t, 1, api.events(t, "app-replace", gateway.TypeWorkspaceKeyReplace))
	})

	t.Run("two approvals at once", func(t *testing.T) {
		requested := api.requestKey(t, keyUser("app-twice"), "dev", newPublicKey(t))

		results := make([]error, 2)

		var group sync.WaitGroup
		for index := range results {
			group.Go(func() { results[index] = api.approveKey(t, "app-twice", requested.Request) })
		}

		group.Wait()

		succeeded := 0

		for _, err := range results {
			if err == nil {
				succeeded++
			} else {
				assert.ErrorIs(t, err, gateway.ErrAccessRequestUndecidable)
			}
		}

		assert.Equal(t, 1, succeeded)
		assert.Equal(t, 1, api.activeKeys(t, "app-twice"))
		assert.Equal(t, 1, api.events(t, "app-twice", gateway.TypeAccessApprove))
	})

	t.Run("approval and revoke at once", func(t *testing.T) {
		requested := api.requestKey(t, keyUser("app-race"), "dev", newPublicKey(t))

		var (
			approveErr error
			revokeErr  error
			revocation gateway.KeyRevocation
			group      sync.WaitGroup
		)

		group.Go(func() { approveErr = api.approveKey(t, "app-race", requested.Request) })
		group.Go(func() {
			revocation, revokeErr = api.workspaces.RevokeKey(t.Context(), keyIdentity("app-race"), "dev", gateway.ViaCLI)
		})
		group.Wait()

		require.NoError(t, revokeErr)
		assert.Zero(t, api.activeKeys(t, "app-race"), "whichever wins, no key stays active")

		if approveErr == nil {
			assert.Equal(t, gateway.KeyRevocation{Revoked: true}, revocation, "the approval won; the revoke revoked the new key")
			assert.Equal(t, "approved", api.requestStatusOf(t, requested.Request))
		} else {
			assert.ErrorIs(t, approveErr, gateway.ErrAccessRequestUndecidable)
			assert.Equal(t, gateway.KeyRevocation{DeniedRequests: 1}, revocation, "the revoke won; it denied the request")
			assert.Equal(t, "denied", api.requestStatusOf(t, requested.Request))
		}
	})

	t.Run("key ID taken meanwhile", func(t *testing.T) {
		key := newPublicKey(t)
		requested := api.requestKey(t, keyUser("app-slow"), "dev", key)
		api.activateKey(t, "app-fast", "dev", key)

		err := api.approveKey(t, "app-slow", requested.Request)

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusConflict, refusal.Status)
		assert.Equal(t, "This key is already registered. Make a new key with a new --key-file.", refusal.Message)
		assert.Equal(t, "pending", api.requestStatusOf(t, requested.Request))
		assert.Zero(t, api.activeKeys(t, "app-slow"))
	})

	t.Run("limit of 20 keys", func(t *testing.T) {
		api.insertActiveKeys(t, "app-limit", gateway.MaxWorkspaceKeys-1)

		last := api.requestKey(t, keyUser("app-limit"), "last", newPublicKey(t))
		over := api.requestKey(t, keyUser("app-limit"), "over", newPublicKey(t))
		require.NoError(t, api.approveKey(t, "app-limit", last.Request), "the 20th key is allowed")

		err := api.approveKey(t, "app-limit", over.Request)

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusConflict, refusal.Status)
		assert.Equal(t, "You already have 20 workspace keys. Revoke one, then approve again.", refusal.Message)
		assert.Equal(t, "pending", api.requestStatusOf(t, over.Request))
		assert.Equal(t, gateway.MaxWorkspaceKeys, api.activeKeys(t, "app-limit"))

		status, body := api.register(t, keyUser("app-limit"), "another", newPublicKey(t))
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "You already have 20 workspace keys. Revoke one first.", body)

		replacement := api.requestKey(t, keyUser("app-limit"), "last", newPublicKey(t))
		require.NoError(t, api.approveKey(t, "app-limit", replacement.Request), "a replacement is not a new key")
		assert.Equal(t, gateway.MaxWorkspaceKeys, api.activeKeys(t, "app-limit"))
	})

	t.Run("deny keeps the active key", func(t *testing.T) {
		active := newPublicKey(t)
		api.activateKey(t, "app-deny", "dev", active)

		requested := api.requestKey(t, keyUser("app-deny"), "dev", newPublicKey(t))
		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			return api.workspaces.DenyAccessRequest(t.Context(), tx, keyIdentity("app-deny"), requested.Request)
		}))

		assert.Equal(t, "denied", api.requestStatusOf(t, requested.Request))
		assert.Equal(t, 1, api.activeKeys(t, "app-deny"))
		assert.Equal(t, 1, api.count(t, `SELECT count(*) FROM workspace_keys WHERE principal = 'app-deny'`))
		assert.Equal(t, 1, api.events(t, "app-deny", gateway.TypeAccessDeny))

		status, _ := api.register(t, keyUser("app-deny"), "dev", active)
		assert.Equal(t, http.StatusOK, status, "the old key is still the active one")
	})

	t.Run("expired request makes no key", func(t *testing.T) {
		requested := api.requestKey(t, keyUser("app-expired"), "dev", newPublicKey(t))
		_, err := fixture.pool.Exec(t.Context(), `UPDATE access_requests SET created_at = now() - interval '1 hour', expires_at = now() - interval '1 second' WHERE id = $1`, requested.Request)
		require.NoError(t, err)

		assert.ErrorIs(t, api.approveKey(t, "app-expired", requested.Request), gateway.ErrAccessRequestUndecidable)
		assert.Equal(t, "pending", api.requestStatusOf(t, requested.Request))
		assert.Zero(t, api.count(t, `SELECT count(*) FROM workspace_keys WHERE principal = 'app-expired'`))
	})

	t.Run("approval while keys are off is refused", func(t *testing.T) {
		requested := api.requestKey(t, keyUser("app-off"), "dev", newPublicKey(t))
		off := newKeyAPI(t, fixture, false)

		err := off.approveKey(t, "app-off", requested.Request)

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusConflict, refusal.Status)
		assert.Equal(t, "Workspace keys are off on this gateway.", refusal.Message)
		assert.Equal(t, "pending", api.requestStatusOf(t, requested.Request))
		assert.Zero(t, api.activeKeys(t, "app-off"))
	})

	t.Run("approval refuses other kinds", func(t *testing.T) {
		id := api.requestChange(t, keyUser("app-kind"), workspacesapi.Change{Action: "defaults", Verified: "all"})

		assert.ErrorIs(t, api.approveKey(t, "app-kind", id), gateway.ErrAccessRequestUndecidable)
	})
}

func TestWorkspaceKeyRevocation(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	t.Run("revoke applies on the next request", func(t *testing.T) {
		key := newWorkspaceKey(t)
		api.activateKey(t, "rev-next", "dev", key.publicKey())

		client := keyUser("rev-next")
		client.session = "next"
		mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
		require.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), mcpSession))

		revocation, err := api.workspaces.RevokeKey(t.Context(), keyIdentity("rev-next"), "dev", gateway.ViaCLI)
		require.NoError(t, err)
		require.True(t, revocation.Revoked)

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, key), mcpSession))
		assert.Equal(t, "attestation key revoked", refusals.last())

		fresh := client
		fresh.session = "next-new"
		fixture.startSession(t, fixture.server.URL, withKey(t, fresh, key))
		fixture.requireSession(t, fresh, "claimed/default", auth.AttestationUser)
	})

	revoke := func(t *testing.T, on *keyAPI, subject, name string) (int, string) {
		t.Helper()

		return on.do(t, ptr(keyUser(subject)), http.MethodDelete, workspacesapi.WorkspaceKeysPath+"/"+name, nil)
	}

	t.Run("revoke denies pending requests", func(t *testing.T) {
		api.activateKey(t, "rev-deny", "dev", newPublicKey(t))
		pending := api.requestKey(t, keyUser("rev-deny"), "dev", newPublicKey(t))
		other := api.requestKey(t, keyUser("rev-deny"), "other", newPublicKey(t))

		status, body := revoke(t, api, "rev-deny", "dev")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, `{"revoked": true, "deniedRequests": 1}`, body)

		assert.Equal(t, "denied", api.requestStatusOf(t, pending.Request))
		assert.Equal(t, "pending", api.requestStatusOf(t, other.Request))
		assert.Zero(t, api.count(t, `SELECT count(*) FROM workspace_keys WHERE principal = 'rev-deny' AND name = 'dev' AND status = 'active'`))
		assert.Equal(t, 1, api.count(t, `SELECT count(*) FROM workspace_keys
			WHERE principal = 'rev-deny' AND name = 'dev' AND status = 'revoked' AND ended_at IS NOT NULL`))
		assert.Equal(t, 1, api.events(t, "rev-deny", gateway.TypeAccessDeny))
		assert.Equal(t, 1, api.events(t, "rev-deny", gateway.TypeWorkspaceKeyRevoke))
		assert.ErrorIs(t, api.approveKey(t, "rev-deny", pending.Request), gateway.ErrAccessRequestUndecidable)

		status, body = revoke(t, api, "rev-deny", "dev")
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, "You have no key named `dev`.", body)
	})

	t.Run("revoke works while the setting is off", func(t *testing.T) {
		api.activateKey(t, "rev-off", "dev", newPublicKey(t))
		off := newKeyAPI(t, fixture, false)

		status, body := off.do(t, ptr(keyUser("rev-off")), http.MethodGet, workspacesapi.WorkspaceKeysPath, nil)
		require.Equal(t, http.StatusOK, status, body)

		var list workspacesapi.WorkspaceKeyList
		require.NoError(t, json.Unmarshal([]byte(body), &list))
		assert.False(t, list.Enabled)
		require.Len(t, list.Keys, 1)

		status, body = revoke(t, off, "rev-off", "dev")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, `{"revoked": true, "deniedRequests": 0}`, body)
		assert.Zero(t, api.activeKeys(t, "rev-off"))
	})

	t.Run("revoke from the Workspaces page", func(t *testing.T) {
		api.activateKey(t, "rev-ui", "dev", newPublicKey(t))
		pending := api.requestKey(t, keyUser("rev-ui"), "dev", newPublicKey(t))

		revocation, err := api.workspaces.RevokeKey(t.Context(), keyIdentity("rev-ui"), "dev", gateway.ViaUI)
		require.NoError(t, err)
		assert.Equal(t, gateway.KeyRevocation{Revoked: true, DeniedRequests: 1}, revocation)
		assert.Zero(t, api.activeKeys(t, "rev-ui"))
		assert.Equal(t, "denied", api.requestStatusOf(t, pending.Request))
		assert.Equal(t, 1, api.count(t, `SELECT count(*) FROM audit_events
			WHERE principal = 'rev-ui' AND type = $1 AND details->>'via' = 'ui'`, gateway.TypeWorkspaceKeyRevoke))
	})

	t.Run("list shows active keys and pending requests by name and status", func(t *testing.T) {
		api.activateKey(t, "rev-list", "b", newPublicKey(t))
		pendingB := api.requestKey(t, keyUser("rev-list"), "b", newPublicKey(t))
		pendingA := api.requestKey(t, keyUser("rev-list"), "a", newPublicKey(t))

		status, body := api.do(t, ptr(keyUser("rev-list")), http.MethodGet, workspacesapi.WorkspaceKeysPath, nil)
		require.Equal(t, http.StatusOK, status, body)

		var list workspacesapi.WorkspaceKeyList
		require.NoError(t, json.Unmarshal([]byte(body), &list))
		assert.True(t, list.Enabled)
		require.Len(t, list.Keys, 3)

		assert.Equal(t, []string{"a/pending", "b/active", "b/pending"}, []string{
			list.Keys[0].Name + "/" + list.Keys[0].Status, list.Keys[1].Name + "/" + list.Keys[1].Status,
			list.Keys[2].Name + "/" + list.Keys[2].Status,
		})
		assert.Equal(t, pendingA.Request, *list.Keys[0].Request)
		assert.Nil(t, list.Keys[0].ApprovedAt)
		assert.Nil(t, list.Keys[1].Request)
		assert.NotNil(t, list.Keys[1].ApprovedAt)
		assert.Nil(t, list.Keys[1].LastUsedAt)
		assert.Equal(t, "registered/rev-list/b", list.Keys[1].Workspace)
		assert.Equal(t, pendingB.Request, *list.Keys[2].Request)
	})
}
