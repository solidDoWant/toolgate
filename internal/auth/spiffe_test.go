package auth_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

// signingKey is a JWT-SVID signing key of a test trust domain.
type signingKey struct {
	id      string
	private *ecdsa.PrivateKey
}

func newSigningKey(t *testing.T, id string) signingKey {
	t.Helper()

	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	return signingKey{id: id, private: private}
}

// svidClaims are the claims of a test JWT-SVID.
type svidClaims struct {
	subject  string
	audience []string
	expiry   time.Time
}

// sign makes a JWT-SVID with go-jose.
func (k signingKey) sign(t *testing.T, claims svidClaims) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: k.private},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), k.id))
	require.NoError(t, err)

	audience := claims.audience
	if audience == nil {
		audience = []string{auth.SPIFFEAudience}
	}

	expiry := claims.expiry
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}

	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  claims.subject,
		Audience: audience,
		Expiry:   jwt.NewNumericDate(expiry),
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}).Serialize()
	require.NoError(t, err)

	return token
}

// bundleEndpoint serves a trust domain's SPIFFE bundle over HTTPS.
type bundleEndpoint struct {
	server      *httptest.Server
	caFile      string
	trustDomain spiffeid.TrustDomain

	mutex   sync.Mutex
	keys    []signingKey
	failing bool
	fetches atomic.Int32
}

func startBundleEndpoint(t *testing.T, trustDomain string, keys ...signingKey) *bundleEndpoint {
	t.Helper()

	endpoint := &bundleEndpoint{trustDomain: spiffeid.RequireTrustDomainFromString(trustDomain), keys: keys}
	endpoint.server = httptest.NewTLSServer(http.HandlerFunc(endpoint.serve))
	t.Cleanup(endpoint.server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.server.Certificate().Raw})
	endpoint.caFile = filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(endpoint.caFile, certificate, 0o600))

	return endpoint
}

func (e *bundleEndpoint) serve(writer http.ResponseWriter, _ *http.Request) {
	e.fetches.Add(1)

	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.failing {
		http.Error(writer, "down", http.StatusInternalServerError)

		return
	}

	bundle := spiffebundle.New(e.trustDomain)
	for _, key := range e.keys {
		if err := bundle.AddJWTAuthority(key.id, crypto.PublicKey(key.private.Public())); err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)

			return
		}
	}

	document, err := bundle.Marshal()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)

		return
	}

	_, _ = writer.Write(document)
}

func (e *bundleEndpoint) setKeys(keys ...signingKey) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.keys = keys
}

func (e *bundleEndpoint) setFailing(failing bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.failing = failing
}

func (e *bundleEndpoint) trustDomainConfig() auth.SPIFFETrustDomain {
	return auth.SPIFFETrustDomain{Name: e.trustDomain.Name(), BundleEndpointURL: e.server.URL, CAFile: e.caFile}
}

// fakeClock is a settable clock for bundle ages.
type fakeClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.now
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.now = c.now.Add(duration)
}

func newSPIFFEFixture(t *testing.T, clock *fakeClock, domains ...auth.SPIFFETrustDomain) fixture {
	t.Helper()

	return newFixture(t, auth.Options{SPIFFE: &auth.SPIFFEOptions{TrustDomains: domains, Now: clock.Now}}, false)
}

// waitForBundle waits until token signs in, which needs the first bundle
// fetch.
func waitForBundle(t *testing.T, f fixture, token string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Equal(collect, http.StatusOK, f.do(t, token, nil).Code)
	}, 10*time.Second, 20*time.Millisecond)
}

const allowedID = "spiffe://allowed.test/owner/alice/workspace/dev"

func TestSVIDBuildsAttestedIdentity(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())

	expiry := time.Now().Add(23 * time.Minute).Truncate(time.Second)
	token := key.sign(t, svidClaims{subject: allowedID, expiry: expiry})
	waitForBundle(t, f, token)

	// The workspace header doesn't change an attested workspace.
	response := f.do(t, token, map[string]string{auth.WorkspaceHeader: "other"})
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, strconv.FormatInt(expiry.Unix(), 10), response.Header().Get("X-Test-Expiry"))

	var identity auth.Identity
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &identity))
	assert.Equal(t, auth.Identity{
		Principal:   allowedID,
		Workspace:   "/owner/alice/workspace/dev",
		Kind:        auth.KindSPIFFE,
		Attestation: auth.AttestationAttested,
	}, identity)
}

func TestBadSVIDsAreRefused(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	otherKey := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())
	waitForBundle(t, f, key.sign(t, svidClaims{subject: allowedID}))

	tests := []struct {
		name        string
		token       string
		reason      string
		trustDomain string
	}{
		{
			name:        "wrong audience",
			token:       key.sign(t, svidClaims{subject: allowedID, audience: []string{"other"}}),
			reason:      "SVID audience not toolgate",
			trustDomain: "allowed.test",
		},
		{
			name:        "expired",
			token:       key.sign(t, svidClaims{subject: allowedID, expiry: time.Now().Add(-time.Minute)}),
			reason:      "SVID expired",
			trustDomain: "allowed.test",
		},
		{
			name:        "wrong signature",
			token:       otherKey.sign(t, svidClaims{subject: allowedID}),
			reason:      "invalid SVID",
			trustDomain: "allowed.test",
		},
		{
			name:        "unknown trust domain",
			token:       key.sign(t, svidClaims{subject: "spiffe://other.test/owner/alice/workspace/dev"}),
			reason:      "SPIFFE trust domain not allowed",
			trustDomain: "other.test",
		},
		{
			name:        "ID over 2048 bytes",
			token:       key.sign(t, svidClaims{subject: "spiffe://allowed.test/" + strings.Repeat("a", auth.MaxSPIFFEIDBytes-len("spiffe://allowed.test/")+1)}),
			reason:      "SPIFFE ID too long",
			trustDomain: "",
		},
		{
			name:        "empty path",
			token:       key.sign(t, svidClaims{subject: "spiffe://allowed.test"}),
			reason:      "SPIFFE ID has an empty path",
			trustDomain: "allowed.test",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(f.recorder.all())
			response := f.do(t, test.token, nil)
			require.Equal(t, http.StatusUnauthorized, response.Code)

			refusals := f.recorder.all()
			require.Len(t, refusals, before+1)
			assert.Equal(t, test.reason, refusals[before].Reason)
			assert.Equal(t, test.trustDomain, refusals[before].TrustDomain)
			assert.NotContains(t, refusals[before].Reason, test.token)
		})
	}
}

func TestSVIDOfExactly2048BytesIsAccepted(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())

	prefix := "spiffe://allowed.test/"
	id := prefix + strings.Repeat("a", auth.MaxSPIFFEIDBytes-len(prefix))
	require.Len(t, id, auth.MaxSPIFFEIDBytes)

	waitForBundle(t, f, key.sign(t, svidClaims{subject: id}))
}

// Row 79: a failing fetch keeps the last good bundle for 24 h.
func TestLastGoodBundleVerifiesFor24Hours(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	clock := &fakeClock{now: time.Now()}
	f := newSPIFFEFixture(t, clock, endpoint.trustDomainConfig())

	token := key.sign(t, svidClaims{subject: allowedID})
	waitForBundle(t, f, token)

	endpoint.setFailing(true)

	clock.advance(24 * time.Hour)
	assert.Equal(t, http.StatusOK, f.do(t, token, nil).Code)

	clock.advance(time.Second)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, token, nil).Code)

	refusals := f.recorder.all()
	require.NotEmpty(t, refusals)
	assert.Equal(t, "no SPIFFE bundle", refusals[len(refusals)-1].Reason)
}

func TestSVIDsAreRefusedUntilTheFirstFetchSucceeds(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	endpoint.setFailing(true)

	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())
	token := key.sign(t, svidClaims{subject: allowedID})

	require.Eventually(t, func() bool { return endpoint.fetches.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, token, nil).Code)

	// Fetch failures are retried with backoff, so a recovered endpoint
	// starts working.
	endpoint.setFailing(false)
	waitForBundle(t, f, token)
}

// Row 80: an unknown key ID fetches the bundle once, at most every 30 s.
func TestRotatedKeyFetchesTheBundleAtMostEvery30Seconds(t *testing.T) {
	t.Parallel()

	oldKey := newSigningKey(t, "old")
	newKey := newSigningKey(t, "new")
	endpoint := startBundleEndpoint(t, "allowed.test", oldKey)
	clock := &fakeClock{now: time.Now()}
	f := newSPIFFEFixture(t, clock, endpoint.trustDomainConfig())
	waitForBundle(t, f, oldKey.sign(t, svidClaims{subject: allowedID}))

	endpoint.setKeys(oldKey, newKey)
	rotated := newKey.sign(t, svidClaims{subject: allowedID})

	fetches := endpoint.fetches.Load()

	clock.advance(29 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, rotated, nil).Code)
	assert.Equal(t, fetches, endpoint.fetches.Load(), "no fetch within 30 s of the last one")

	refusals := f.recorder.all()
	require.NotEmpty(t, refusals)
	assert.Equal(t, auth.Refusal{
		Reason: "unknown SPIFFE key ID", TrustDomain: "allowed.test", RemoteAddress: "192.0.2.10",
	}, refusals[len(refusals)-1])

	clock.advance(time.Second)
	assert.Equal(t, http.StatusOK, f.do(t, rotated, nil).Code)
	assert.Equal(t, fetches+1, endpoint.fetches.Load(), "one fetch for the unknown key ID")
}

// Row 82: an IdP token with a spiffe:// subject goes to the SPIFFE
// verifier and is refused.
func TestIdPTokenWithSPIFFESubjectIsRefused(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())
	waitForBundle(t, f, key.sign(t, svidClaims{subject: allowedID}))

	for _, subject := range []string{allowedID, "spiffe://other.test/owner/alice/workspace/dev"} {
		token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: subject})
		assert.Equal(t, http.StatusUnauthorized, f.do(t, token, nil).Code, subject)
	}
}

// Row 84: trust domains are read at start, so one left out of the
// configuration refuses its SVIDs.
func TestRemovedTrustDomainIsRefused(t *testing.T) {
	t.Parallel()

	allowedKey := newSigningKey(t, "key-1")
	removedKey := newSigningKey(t, "key-2")
	allowed := startBundleEndpoint(t, "allowed.test", allowedKey)
	removed := startBundleEndpoint(t, "removed.test", removedKey)

	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, allowed.trustDomainConfig())
	waitForBundle(t, f, allowedKey.sign(t, svidClaims{subject: allowedID}))

	token := removedKey.sign(t, svidClaims{subject: "spiffe://removed.test/owner/alice/workspace/dev"})
	assert.Equal(t, http.StatusUnauthorized, f.do(t, token, nil).Code)
	assert.Equal(t, int32(0), removed.fetches.Load())
}

func TestBundleAgeMetric(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	clock := &fakeClock{now: time.Now()}
	f := newSPIFFEFixture(t, clock, endpoint.trustDomainConfig())
	waitForBundle(t, f, key.sign(t, svidClaims{subject: allowedID}))

	clock.advance(90 * time.Second)

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(f.authenticator.Collectors()[1]))
	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP toolgate_spiffe_bundle_age_seconds Seconds since the trust domain's SPIFFE bundle was last fetched successfully.
# TYPE toolgate_spiffe_bundle_age_seconds gauge
toolgate_spiffe_bundle_age_seconds{trust_domain="allowed.test"} 90
`), "toolgate_spiffe_bundle_age_seconds"))
}

func TestJWKSTrustDomain(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, auth.SPIFFETrustDomain{
		Name: "allowed.test", JWKSURL: endpoint.server.URL, CAFile: endpoint.caFile,
	})

	waitForBundle(t, f, key.sign(t, svidClaims{subject: allowedID}))
}
