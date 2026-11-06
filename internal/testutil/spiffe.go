package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
)

// SPIFFETrustDomain is a test SPIFFE trust domain with one JWT-SVID signing
// key. Signing uses go-jose; the gateway verifies with go-spiffe.
type SPIFFETrustDomain struct {
	TrustDomain spiffeid.TrustDomain
	keyID       string
	key         *ecdsa.PrivateKey
}

// NewSPIFFETrustDomain makes a trust domain, such as `allowed.test`, with a
// fresh signing key.
func NewSPIFFETrustDomain(tb testing.TB, name string) *SPIFFETrustDomain {
	tb.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)

	return &SPIFFETrustDomain{
		TrustDomain: spiffeid.RequireTrustDomainFromString(name),
		keyID:       "key-" + newSerial(tb).Text(36)[:8],
		key:         key,
	}
}

// BundleHandler serves the trust domain's SPIFFE bundle, as a bundle
// endpoint does.
func (d *SPIFFETrustDomain) BundleHandler(tb testing.TB) http.Handler {
	tb.Helper()

	bundle := spiffebundle.New(d.TrustDomain)
	require.NoError(tb, bundle.AddJWTAuthority(d.keyID, d.key.Public()))
	document, err := bundle.Marshal()
	require.NoError(tb, err)

	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(document)
	})
}

// MintSVID signs a JWT-SVID for path in the trust domain, such as
// `/owner/alice/workspace/dev`, with the audience `toolgate`, valid for an
// hour.
func (d *SPIFFETrustDomain) MintSVID(tb testing.TB, path string) string {
	tb.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: d.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), d.keyID))
	require.NoError(tb, err)

	now := time.Now()
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  "spiffe://" + d.TrustDomain.Name() + path,
		Audience: jwt.Audience{"toolgate"},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	require.NoError(tb, err)

	return token
}
