package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// fakeKeyStore is a WorkspaceKeyStore over a map, recording each call.
type fakeKeyStore struct {
	mutex   sync.Mutex
	rows    map[string]auth.WorkspaceKey
	pending map[string]string // key ID to principal
	failGet bool
	// failPending fails HasPendingKeyRequest.
	failPending bool
	failTouch   bool
	lookups     int
	touched     []int64
}

func (s *fakeKeyStore) WorkspaceKey(_ context.Context, keyID string) (auth.WorkspaceKey, bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.lookups++

	if s.failGet {
		return auth.WorkspaceKey{}, false, errors.New("connection refused")
	}

	row, ok := s.rows[keyID]

	return row, ok, nil
}

func (s *fakeKeyStore) HasPendingKeyRequest(_ context.Context, principal, keyID string) (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.failPending {
		return false, errors.New("connection refused")
	}

	return s.pending[keyID] == principal, nil
}

func (s *fakeKeyStore) TouchWorkspaceKey(_ context.Context, id int64) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.touched = append(s.touched, id)

	if s.failTouch {
		return errors.New("connection refused")
	}

	return nil
}

// workspaceKeyPair is a key pair a test registers.
type workspaceKeyPair struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
	id      string
}

func newWorkspaceKeyPair(t *testing.T) workspaceKeyPair {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	thumbprint, err := (&jose.JSONWebKey{Key: public}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)

	return workspaceKeyPair{public: public, private: private, id: base64.RawURLEncoding.EncodeToString(thumbprint)}
}

// assertionClaims are the claims of a test assertion. Nil times are left
// out.
type assertionClaims struct {
	Audience  jwt.Audience     `json:"aud,omitempty"`
	IssuedAt  *jwt.NumericDate `json:"iat,omitempty"`
	Expiry    *jwt.NumericDate `json:"exp,omitempty"`
	NotBefore *jwt.NumericDate `json:"nbf,omitempty"`
	ID        string           `json:"jti,omitempty"`
}

// validAssertion is what the client sends at now.
func validAssertion(now time.Time) assertionClaims {
	return assertionClaims{
		Audience: jwt.Audience{"toolgate"},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(300 * time.Second)),
		ID:       "c2VjcmV0LW5vbmNlLTEyMw",
	}
}

// sign makes an assertion with the given key, key ID and claims.
func signAssertion(t *testing.T, signingKey jose.SigningKey, keyID string, claims assertionClaims) string {
	t.Helper()

	options := (&jose.SignerOptions{}).WithType(auth.WorkspaceKeyAssertionType).WithHeader(jose.HeaderKey("kid"), keyID)

	signer, err := jose.NewSigner(signingKey, options)
	require.NoError(t, err)

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	return token
}

func (k workspaceKeyPair) sign(t *testing.T, claims assertionClaims) string {
	t.Helper()

	return signAssertion(t, jose.SigningKey{Algorithm: jose.EdDSA, Key: k.private}, k.id, claims)
}

func TestWorkspaceKeyVerdicts(t *testing.T) {
	t.Parallel()

	now := time.Now().Truncate(time.Second)
	key := newWorkspaceKeyPair(t)
	other := newWorkspaceKeyPair(t)

	activeRow := func() auth.WorkspaceKey {
		return auth.WorkspaceKey{
			ID: 7, Principal: "alice-sub", Username: "alice", Name: "dev",
			Status: "active", PublicKey: key.public,
		}
	}

	withRow := func(row auth.WorkspaceKey) func(*fakeKeyStore) {
		return func(store *fakeKeyStore) { store.rows[key.id] = row }
	}

	lastUsed := func(ago time.Duration) *time.Time {
		used := now.Add(-ago)

		return &used
	}

	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tests := []struct {
		name string
		// assertion returns the header value; server issues pod tokens.
		assertion func(t *testing.T, server *fakeAPIServer) string
		store     func(*fakeKeyStore)
		// podAttestation turns workspaceAttestation on.
		podAttestation bool
		// keysOff turns workspaceKeys off.
		keysOff       bool
		prefix        string
		wantStatus    int
		wantAttested  bool
		wantWorkspace string
		// wantReason is the reason of the one recorded auth.refuse; empty
		// expects none.
		wantReason string
		wantTouch  bool
		// wantNoLookup expects the key store to be left alone.
		wantNoLookup bool
	}{
		{
			name:          "approved key attests with the registered workspace",
			assertion:     func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store:         withRow(activeRow()),
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name:      "pending key is unverified",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				store.pending[key.id] = "alice-sub"
			},
			wantReason: "attestation key not approved",
		},
		{
			name:      "unknown key is unverified",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				// A pending request of another user doesn't count.
				store.pending[key.id] = "bob-sub"
			},
			wantReason: "attestation key unknown",
		},
		{
			name:      "revoked key is unverified",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				row := activeRow()
				row.Status = "revoked"
				store.rows[key.id] = row
			},
			wantReason: "attestation key revoked",
		},
		{
			name:      "replaced key is unverified",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				row := activeRow()
				row.Status = "replaced"
				store.rows[key.id] = row
			},
			wantReason: "attestation key revoked",
		},
		{
			name:      "owner mismatch",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				row := activeRow()
				row.Principal = "bob-sub"
				// The owner check comes before the status check.
				row.Status = "revoked"
				store.rows[key.id] = row
			},
			wantReason: "attestation key owner mismatch",
		},
		{
			name: "bad signature",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				// Signed by another key under the registered key's ID.
				return signAssertion(t, jose.SigningKey{Algorithm: jose.EdDSA, Key: other.private}, key.id, validAssertion(now))
			},
			store: func(store *fakeKeyStore) {
				row := activeRow()
				// The signature check comes before the owner check.
				row.Principal = "bob-sub"
				store.rows[key.id] = row
			},
			wantReason: "attestation assertion signature invalid",
		},
		{
			name: "expired assertion",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return key.sign(t, validAssertion(now.Add(-331*time.Second)))
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion expired",
		},
		{
			name: "expired assertion within the leeway attests",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return key.sign(t, validAssertion(now.Add(-330*time.Second)))
			},
			store:         withRow(activeRow()),
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name: "lifetime over five minutes",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.Expiry = jwt.NewNumericDate(now.Add(301 * time.Second))

				return key.sign(t, claims)
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion lifetime too long",
		},
		{
			name: "wrong audience",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.Audience = jwt.Audience{"toolgate-other"}

				return key.sign(t, claims)
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion audience wrong",
		},
		{
			name: "issued in the future",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return key.sign(t, validAssertion(now.Add(31*time.Second)))
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion issued in the future",
		},
		{
			name: "issued in the future within the leeway attests",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return key.sign(t, validAssertion(now.Add(30*time.Second)))
			},
			store:         withRow(activeRow()),
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name: "missing jti",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.ID = ""

				return key.sign(t, claims)
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion invalid",
		},
		{
			name: "jti over 128 characters",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.ID = strings.Repeat("a", 129)

				return key.sign(t, claims)
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion invalid",
		},
		{
			name: "jti of 128 characters attests",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.ID = strings.Repeat("a", 128)

				return key.sign(t, claims)
			},
			store:         withRow(activeRow()),
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name: "missing iat",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				claims := validAssertion(now)
				claims.IssuedAt = nil

				return key.sign(t, claims)
			},
			store:      withRow(activeRow()),
			wantReason: "attestation assertion invalid",
		},
		{
			name: "algorithm other than EdDSA",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return signAssertion(t, jose.SigningKey{Algorithm: jose.ES256, Key: ecdsaKey}, key.id, validAssertion(now))
			},
			store:        withRow(activeRow()),
			wantReason:   "attestation assertion invalid",
			wantNoLookup: true,
		},
		{
			name: "key ID that isn't a thumbprint",
			assertion: func(t *testing.T, _ *fakeAPIServer) string {
				return signAssertion(t, jose.SigningKey{Algorithm: jose.EdDSA, Key: key.private}, "short", validAssertion(now))
			},
			store:        withRow(activeRow()),
			wantReason:   "attestation assertion invalid",
			wantNoLookup: true,
		},
		{
			name:         "workspace keys off",
			assertion:    func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store:        withRow(activeRow()),
			keysOff:      true,
			wantNoLookup: true,
		},
		{
			name:           "key assertion while only workspaceAttestation is on",
			assertion:      func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store:          withRow(activeRow()),
			podAttestation: true,
			keysOff:        true,
			wantReason:     "attestation workspace keys off",
			wantNoLookup:   true,
		},
		{
			name:      "no touch within a minute of the last use",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				row := activeRow()
				row.LastUsedAt = lastUsed(time.Minute)
				store.rows[key.id] = row
			},
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
		},
		{
			name:      "touch over a minute after the last use",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				row := activeRow()
				row.LastUsedAt = lastUsed(time.Minute + time.Second)
				store.rows[key.id] = row
			},
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name:          "prefix is read from the configuration",
			assertion:     func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store:         withRow(activeRow()),
			prefix:        "laptops",
			wantAttested:  true,
			wantWorkspace: "laptops/alice/dev",
			wantTouch:     true,
		},
		{
			name:      "store error gives unavailable",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				store.failGet = true
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:      "pending check error gives unavailable",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				store.failPending = true
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:      "touch failure keeps the attestation",
			assertion: func(t *testing.T, _ *fakeAPIServer) string { return key.sign(t, validAssertion(now)) },
			store: func(store *fakeKeyStore) {
				store.rows[key.id] = activeRow()
				store.failTouch = true
			},
			wantAttested:  true,
			wantWorkspace: "registered/alice/dev",
			wantTouch:     true,
		},
		{
			name: "pod token while only workspace keys are on",
			assertion: func(t *testing.T, server *fakeAPIServer) string {
				return server.issue(t, workspaceToken)
			},
			store:        withRow(activeRow()),
			wantReason:   "attestation token invalid",
			wantNoLookup: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeKeyStore{rows: map[string]auth.WorkspaceKey{}, pending: map[string]string{}}
			if test.store != nil {
				test.store(store)
			}

			server := newFakeAPIServer(t, workspacePod(nil))

			options := auth.Options{}
			if test.podAttestation {
				options.Attestation = emailAttestation(server)
			}

			if !test.keysOff {
				prefix := test.prefix
				if prefix == "" {
					prefix = "registered"
				}

				options.WorkspaceKeys = &auth.WorkspaceKeyOptions{
					Store: store, WorkspaceIDPrefix: prefix, Now: func() time.Time { return now },
				}
			}

			f := newFixture(t, options, false)

			response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{
				auth.AttestationHeader: test.assertion(t, server),
				auth.WorkspaceHeader:   "elsewhere",
			})

			wantStatus := test.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}

			require.Equal(t, wantStatus, response.Code, response.Body.String())

			if wantStatus == http.StatusServiceUnavailable {
				assert.Equal(t, "1", response.Header().Get("Retry-After"))
				assert.Empty(t, f.recorder.all(), "an unavailable store writes no auth.refuse")

				return
			}

			identity := decodeIdentity(t, response.Body.Bytes())
			assert.Equal(t, "alice-sub", identity.Principal)
			assert.Equal(t, auth.KindUser, identity.Kind)

			if test.wantAttested {
				assert.Equal(t, auth.AttestationAttested, identity.Attestation)
				assert.Equal(t, test.wantWorkspace, identity.Workspace)
			} else {
				assert.Equal(t, auth.AttestationUser, identity.Attestation)
				assert.Equal(t, "claimed/elsewhere", identity.Workspace)
			}

			refusals := f.recorder.all()
			if test.wantReason == "" {
				assert.Empty(t, refusals)
			} else {
				require.Len(t, refusals, 1)
				assert.Equal(t, auth.Refusal{
					Principal: "alice-sub", Reason: test.wantReason, RemoteAddress: "192.0.2.10",
				}, refusals[0], "a key refusal names no issuer, audience or trust domain")
			}

			if test.wantTouch {
				assert.Equal(t, []int64{7}, store.touched)
			} else {
				assert.Empty(t, store.touched)
			}

			if test.wantNoLookup {
				assert.Zero(t, store.lookups, "the key store isn't read")
			}

			assert.Zero(t, server.reviews.Load(), "a key assertion never reaches TokenReview")
		})
	}
}
