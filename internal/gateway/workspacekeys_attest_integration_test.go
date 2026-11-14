//go:build integration

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/client/attestation"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
)

// refusalLog collects the reasons of the auth.refuse events an authenticator
// records.
type refusalLog struct {
	mutex   sync.Mutex
	reasons []string
}

func (l *refusalLog) RecordAuthRefusal(_ context.Context, refusal auth.Refusal) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	l.reasons = append(l.reasons, refusal.Reason)
}

// last returns the reason of the newest refusal, or "".
func (l *refusalLog) last() string {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if len(l.reasons) == 0 {
		return ""
	}

	return l.reasons[len(l.reasons)-1]
}

func (l *refusalLog) count() int {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	return len(l.reasons)
}

// lateKeyStore reads key rows from a pool that exists only after the
// fixture has built its authenticator.
type lateKeyStore struct {
	pool func() *pgxpool.Pool
}

func (s lateKeyStore) keys() *store.WorkspaceKeys { return store.NewWorkspaceKeys(s.pool()) }

func (s lateKeyStore) WorkspaceKey(ctx context.Context, keyID string) (auth.WorkspaceKey, bool, error) {
	row, found, err := s.keys().WorkspaceKey(ctx, keyID)
	if err != nil || !found {
		return auth.WorkspaceKey{}, found, err
	}

	return auth.WorkspaceKey{
		ID: row.ID, Principal: row.Principal, Username: row.Username, Name: row.Name,
		Status: row.Status, PublicKey: ed25519.PublicKey(row.PublicKey), LastUsedAt: row.LastUsedAt,
	}, true, nil
}

func (s lateKeyStore) HasPendingKeyRequest(ctx context.Context, principal, keyID string) (bool, error) {
	return s.keys().HasPendingKeyRequest(ctx, principal, keyID)
}

func (s lateKeyStore) TouchWorkspaceKey(ctx context.Context, id int64) error {
	return s.keys().TouchWorkspaceKey(ctx, id)
}

// newKeyFixture is an MCP fixture whose authenticator verifies pod tokens
// and workspace key assertions, and records every refusal.
func newKeyFixture(t *testing.T) (*mcpFixture, *refusalLog) {
	t.Helper()

	refusals := &refusalLog{}

	var fixture *mcpFixture

	options := attestedUsers()
	options.Recorder = refusals
	options.WorkspaceKeys = &auth.WorkspaceKeyOptions{
		Store:             lateKeyStore{pool: func() *pgxpool.Pool { return fixture.pool }},
		WorkspaceIDPrefix: gateway.DefaultWorkspaceKeysPrefix,
	}

	fixture = newMCPFixtureWithAuth(t, options)

	return fixture, refusals
}

// restartWith replaces the fixture's authenticator, as a restart with other
// settings does, and starts a replica that uses it. The fixture's server is
// that replica.
func (f *mcpFixture) restartWith(t *testing.T, options *auth.Options) {
	t.Helper()

	options.IssuerURL, options.Audiences = f.provider.Issuer, []string{f.provider.Audience}

	authenticator, err := auth.New(t.Context(), *options)
	require.NoError(t, err)

	f.authenticator = authenticator
	f.server = f.replica(t)
}

// workspaceKey is a client's Ed25519 key pair.
type workspaceKey struct {
	private ed25519.PrivateKey
	id      string
}

func newWorkspaceKey(t *testing.T) workspaceKey {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	id, err := attestation.KeyID(public)
	require.NoError(t, err)

	return workspaceKey{private: private, id: id}
}

func (k workspaceKey) publicKey() string {
	return base64.RawURLEncoding.EncodeToString(k.private.Public().(ed25519.PublicKey))
}

// assertion signs a fresh assertion, as `toolgate headers` does for each run.
func (k workspaceKey) assertion(t *testing.T) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: k.private},
		(&jose.SignerOptions{}).WithType(attestation.AssertionType).WithHeader(jose.HeaderKey("kid"), k.id))
	require.NoError(t, err)

	nonce := make([]byte, 16)
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	now := time.Now().Truncate(time.Second)
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Audience: jwt.Audience{auth.WorkspaceKeyAudience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(attestation.AssertionLifetime)),
		ID:       base64.RawURLEncoding.EncodeToString(nonce),
	}).Serialize()
	require.NoError(t, err)

	return token
}

// withKey is client sending an assertion of key.
func withKey(t *testing.T, client user, key workspaceKey) user {
	t.Helper()

	client.attestation = key.assertion(t)

	return client
}

// startSession opens an MCP session and returns its ID.
func (f *mcpFixture) startSession(t *testing.T, url string, client user) string {
	t.Helper()

	response := f.postTo(t, url, client, "", initializeMessage())
	require.Equal(t, http.StatusOK, response.StatusCode)

	id := response.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, id)

	return id
}

// requireSession checks the workspace and attestation of a recorded session.
func (f *mcpFixture) requireSession(t *testing.T, client user, workspace, attestation string) {
	t.Helper()

	row := f.sessionRow(t, client.claims.Subject, client.session)
	assert.Equal(t, workspace, row.Workspace)
	assert.Equal(t, attestation, row.Attestation)
}

func TestWorkspaceKeySessionBinding(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	t.Run("an MCP session keeps its workspace across new assertions", func(t *testing.T) {
		key := newWorkspaceKey(t)
		api.activateKey(t, "bind-keep", "dev", key.publicKey())

		client := keyUser("bind-keep")
		client.session = "keep"
		mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
		fixture.requireSession(t, client, "registered/bind-keep/dev", auth.AttestationAttested)

		for range 3 {
			assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), mcpSession))
		}
	})

	t.Run("a key with another name gets 404", func(t *testing.T) {
		dev, other := newWorkspaceKey(t), newWorkspaceKey(t)
		api.activateKey(t, "bind-name", "dev", dev.publicKey())
		api.activateKey(t, "bind-name", "other", other.publicKey())

		client := keyUser("bind-name")
		client.session = "name"
		mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, dev))

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, other), mcpSession))
		assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, dev), mcpSession))
	})

	t.Run("approval during an unverified session gives 404", func(t *testing.T) {
		key := newWorkspaceKey(t)
		pending := api.requestKey(t, keyUser("bind-late"), "dev", key.publicKey())

		client := keyUser("bind-late")
		client.session = "late"
		mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
		assert.Equal(t, "attestation key not approved", refusals.last())
		fixture.requireSession(t, client, "claimed/default", auth.AttestationUser)

		require.NoError(t, api.approveKey(t, "bind-late", pending.Request))

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, key), mcpSession))

		fresh := client
		fresh.session = "late-new"
		fixture.startSession(t, fixture.server.URL, withKey(t, fresh, key))
		fixture.requireSession(t, fresh, "registered/bind-late/dev", auth.AttestationAttested)
	})
}

func TestWorkspaceKeysOff(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	key := newWorkspaceKey(t)
	api.activateKey(t, "off-user", "dev", key.publicKey())

	client := keyUser("off-user")
	client.session = "off"
	mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
	fixture.requireSession(t, client, "registered/off-user/dev", auth.AttestationAttested)
	require.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), mcpSession))

	// A restart with workspace keys off and workload attestation still on.
	offOptions := attestedUsers()
	offOptions.Recorder = refusals
	fixture.restartWith(t, offOptions)

	before := refusals.count()

	assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, key), mcpSession), "a bound session")
	assert.Equal(t, "attestation workspace keys off", refusals.last())
	assert.Greater(t, refusals.count(), before)

	fresh := client
	fresh.session = "off-new"
	fixture.startSession(t, fixture.server.URL, withKey(t, fresh, key))
	fixture.requireSession(t, fresh, "claimed/default", auth.AttestationUser)

	off := newKeyAPI(t, fixture, false)

	status, body := off.register(t, keyUser("off-user"), "again", newPublicKey(t))
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "Workspace keys are off on this gateway.", body)

	status, _ = off.do(t, ptr(keyUser("off-user")), http.MethodGet, "/api/v1/workspace-keys", nil)
	assert.Equal(t, http.StatusOK, status, "list works while off")

	// The rows stay, so turning the setting on again makes the key work.
	onOptions := attestedUsers()
	onOptions.Recorder = refusals
	onOptions.WorkspaceKeys = &auth.WorkspaceKeyOptions{
		Store:             lateKeyStore{pool: func() *pgxpool.Pool { return fixture.pool }},
		WorkspaceIDPrefix: gateway.DefaultWorkspaceKeysPrefix,
	}
	fixture.restartWith(t, onOptions)

	assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), mcpSession))
}

func TestWorkspaceKeysOffWithoutAttestation(t *testing.T) {
	fixture, refusals := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	key := newWorkspaceKey(t)
	api.activateKey(t, "off-plain", "dev", key.publicKey())

	fixture.restartWith(t, &auth.Options{Recorder: refusals})

	client := keyUser("off-plain")
	client.session = "plain"
	mcpSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))

	assert.Zero(t, refusals.count(), "the header is ignored with no event")
	fixture.requireSession(t, client, "claimed/default", auth.AttestationUser)
	assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), mcpSession))
}
