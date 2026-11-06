package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// SPIFFEAudience is the audience a JWT-SVID must carry for the gateway.
const SPIFFEAudience = "toolgate"

// Bundle fetching and SVID limits (docs/design/phase-3.md §4).
const (
	spiffeScheme = "spiffe://"
	// MaxSPIFFEIDBytes is the SPIFFE limit on the length of an ID.
	MaxSPIFFEIDBytes = 2048

	bundleRefreshInterval = 5 * time.Minute
	minBundleFetchGap     = 30 * time.Second
	bundleFetchTimeout    = 10 * time.Second
	maxBundleBytes        = 1 << 20
	bundleTrustTime       = 24 * time.Hour
	minFetchBackoff       = time.Second
	maxFetchBackoff       = 30 * time.Second
)

// SPIFFETrustDomain is a trust domain whose JWT-SVIDs may sign in, and where
// its bundle comes from. Exactly one of BundleEndpointURL and JWKSURL is set.
type SPIFFETrustDomain struct {
	// Name is the trust domain, such as `coder.example.com`.
	Name string
	// BundleEndpointURL is a SPIFFE bundle endpoint with the https_web
	// profile.
	BundleEndpointURL string
	// JWKSURL is a JWKS document, such as that of SPIRE's OIDC discovery
	// provider.
	JWKSURL string
	// CAFile, if set, is a PEM file of private CAs added to the system
	// roots.
	CAFile string
}

// SPIFFEOptions enables sign-in with SPIFFE JWT-SVIDs.
type SPIFFEOptions struct {
	// TrustDomains are the trust domains allowed to sign in.
	TrustDomains []SPIFFETrustDomain
	// Logger receives fetch failures. Nil discards them.
	Logger *slog.Logger
	// Now returns the current time. Nil uses time.Now.
	Now func() time.Time
}

// spiffeVerifier verifies JWT-SVIDs against the bundles of the configured
// trust domains, which it fetches and refreshes in the background.
type spiffeVerifier struct {
	sources map[spiffeid.TrustDomain]*bundleSource
	bundles *jwtbundle.Set
	now     func() time.Time
	logger  *slog.Logger
}

// bundleSource fetches one trust domain's bundle.
type bundleSource struct {
	trustDomain spiffeid.TrustDomain
	fetch       func(ctx context.Context) (*jwtbundle.Bundle, time.Duration, error)

	// fetchMutex serializes fetches. stateMutex guards the times.
	fetchMutex  sync.Mutex
	stateMutex  sync.Mutex
	lastAttempt time.Time
	lastSuccess time.Time
}

func newSPIFFEVerifier(options SPIFFEOptions) (*spiffeVerifier, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	verifier := &spiffeVerifier{
		sources: map[spiffeid.TrustDomain]*bundleSource{},
		bundles: jwtbundle.NewSet(),
		now:     now,
		logger:  logger,
	}

	for _, domain := range options.TrustDomains {
		trustDomain, err := spiffeid.TrustDomainFromString(domain.Name)
		if err != nil {
			return nil, fmt.Errorf("auth: SPIFFE trust domain %q: %w", domain.Name, err)
		}

		client, err := bundleClient(domain.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth: SPIFFE trust domain %s: %w", domain.Name, err)
		}

		source := &bundleSource{trustDomain: trustDomain}

		switch {
		case domain.BundleEndpointURL != "" && domain.JWKSURL == "":
			source.fetch = func(ctx context.Context) (*jwtbundle.Bundle, time.Duration, error) {
				return fetchBundleEndpoint(ctx, client, trustDomain, domain.BundleEndpointURL)
			}
		case domain.JWKSURL != "" && domain.BundleEndpointURL == "":
			source.fetch = func(ctx context.Context) (*jwtbundle.Bundle, time.Duration, error) {
				return fetchJWKS(ctx, client, trustDomain, domain.JWKSURL)
			}
		default:
			return nil, fmt.Errorf("auth: SPIFFE trust domain %s needs exactly one of bundleEndpointURL and jwksURL", domain.Name)
		}

		verifier.sources[trustDomain] = source
	}

	return verifier, nil
}

// bundleClient is an HTTPS client trusting the system roots plus caFile.
func bundleClient(caFile string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading caFile: %w", err)
		}

		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("caFile %s has no PEM certificate", caFile)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}

	return &http.Client{
		Transport: transport,
		// A bundle comes from the configured URL only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// readBundleDocument GETs url over HTTPS, reading at most maxBundleBytes.
func readBundleDocument(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBundleBytes+1))
	if err != nil {
		return nil, err
	}

	if len(body) > maxBundleBytes {
		return nil, fmt.Errorf("GET %s: the response is over 1 MiB", url)
	}

	return body, nil
}

// fetchBundleEndpoint reads a SPIFFE bundle from an https_web bundle
// endpoint. The response is bounded before go-spiffe parses it, which
// federation.FetchBundle can't do.
func fetchBundleEndpoint(ctx context.Context, client *http.Client, trustDomain spiffeid.TrustDomain, url string,
) (*jwtbundle.Bundle, time.Duration, error) {
	body, err := readBundleDocument(ctx, client, url)
	if err != nil {
		return nil, 0, err
	}

	bundle, err := spiffebundle.Parse(trustDomain, body)
	if err != nil {
		return nil, 0, err
	}

	hint, _ := bundle.RefreshHint()

	return bundle.JWTBundle(), hint, nil
}

// fetchJWKS reads a JWKS document as a trust domain's JWT bundle.
func fetchJWKS(ctx context.Context, client *http.Client, trustDomain spiffeid.TrustDomain, url string,
) (*jwtbundle.Bundle, time.Duration, error) {
	body, err := readBundleDocument(ctx, client, url)
	if err != nil {
		return nil, 0, err
	}

	bundle, err := jwtbundle.Parse(trustDomain, body)
	if err != nil {
		return nil, 0, err
	}

	return bundle, 0, nil
}

// fetchNow fetches the trust domain's bundle and returns its refresh hint.
// Unless scheduled is set, it doesn't fetch when a fetch started less than
// 30 s ago. The refresh loop sets scheduled: it keeps its own times.
func (v *spiffeVerifier) fetchNow(ctx context.Context, source *bundleSource, scheduled bool) (time.Duration, error) {
	source.fetchMutex.Lock()
	defer source.fetchMutex.Unlock()

	source.stateMutex.Lock()
	if !scheduled && !source.lastAttempt.IsZero() && v.now().Sub(source.lastAttempt) < minBundleFetchGap {
		source.stateMutex.Unlock()

		return 0, errFetchedRecently
	}

	source.lastAttempt = v.now()
	source.stateMutex.Unlock()

	fetchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), bundleFetchTimeout)
	defer cancel()

	bundle, hint, err := source.fetch(fetchContext)
	if err != nil {
		v.logger.WarnContext(ctx, "cannot fetch the SPIFFE bundle", "trustDomain", source.trustDomain.Name(), "error", err)

		return 0, err
	}

	v.bundles.Add(bundle)

	source.stateMutex.Lock()
	source.lastSuccess = v.now()
	source.stateMutex.Unlock()

	return hint, nil
}

var errFetchedRecently = errors.New("the bundle was fetched less than 30 s ago")

// run refreshes every trust domain's bundle until ctx ends: at once, then
// every 5 minutes or at the bundle's refresh hint if that is shorter, but
// never more often than every 30 s. Failures are retried with backoff from
// 1 s to 30 s.
func (v *spiffeVerifier) run(ctx context.Context) {
	for _, source := range v.sources {
		go v.refreshLoop(ctx, source)
	}
}

func (v *spiffeVerifier) refreshLoop(ctx context.Context, source *bundleSource) {
	backoff := minFetchBackoff

	for {
		hint, err := v.fetchNow(ctx, source, true)

		var delay time.Duration

		switch {
		case err != nil:
			// Jitter keeps replicas from retrying together.
			delay = backoff + rand.N(backoff/2+1) //nolint:gosec // Jitter, not a secret.
			backoff = min(backoff*2, maxFetchBackoff)
		default:
			backoff = minFetchBackoff
			delay = bundleRefreshInterval

			if hint > 0 && hint < delay {
				delay = max(hint, minBundleFetchGap)
			}
		}

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// trusted reports whether the source's bundle may verify SVIDs: it was
// fetched successfully in the last 24 h. A stale bundle is dropped.
func (v *spiffeVerifier) trusted(source *bundleSource) bool {
	source.stateMutex.Lock()
	lastSuccess := source.lastSuccess
	source.stateMutex.Unlock()

	if lastSuccess.IsZero() {
		return false
	}

	if v.now().Sub(lastSuccess) > bundleTrustTime {
		v.bundles.Remove(source.trustDomain)

		return false
	}

	return v.bundles.Has(source.trustDomain)
}

// verify checks a token whose unverified subject starts with spiffe://.
func (v *spiffeVerifier) verify(ctx context.Context, token, subject string) (Identity, *Refusal) {
	if len(subject) > MaxSPIFFEIDBytes {
		return Identity{}, &Refusal{Reason: "SPIFFE ID too long"}
	}

	id, err := spiffeid.FromString(subject)
	if err != nil {
		return Identity{}, &Refusal{Reason: "invalid SPIFFE ID"}
	}

	trustDomain := id.TrustDomain().Name()

	if id.Path() == "" {
		return Identity{}, &Refusal{Reason: "SPIFFE ID has an empty path", TrustDomain: trustDomain}
	}

	source, ok := v.sources[id.TrustDomain()]
	if !ok {
		return Identity{}, &Refusal{Reason: "SPIFFE trust domain not allowed", TrustDomain: trustDomain}
	}

	if !v.trusted(source) {
		return Identity{}, &Refusal{Reason: "no SPIFFE bundle", TrustDomain: trustDomain}
	}

	if keyID := unverifiedKeyID(token); keyID != "" && !v.hasKey(source, keyID) {
		// One fetch, at most every 30 s per trust domain, then verify
		// again. A concurrent fetch may have brought the key too.
		_, _ = v.fetchNow(ctx, source, false)

		if !v.hasKey(source, keyID) {
			return Identity{}, &Refusal{Reason: "unknown SPIFFE key ID", TrustDomain: trustDomain}
		}
	}

	svid, err := jwtsvid.ParseAndValidate(token, v.bundles, []string{SPIFFEAudience})
	if err != nil {
		return Identity{}, &Refusal{Reason: svidRefusalReason(token), TrustDomain: trustDomain}
	}

	// ParseAndValidate reads the subject again; it must be the one routed.
	if svid.ID.String() != id.String() {
		return Identity{}, &Refusal{Reason: "invalid SVID", TrustDomain: trustDomain}
	}

	return Identity{
		Principal:   svid.ID.String(),
		Workspace:   svid.ID.Path(),
		Attestation: AttestationAttested,
	}, nil
}

func (v *spiffeVerifier) hasKey(source *bundleSource, keyID string) bool {
	bundle, ok := v.bundles.Get(source.trustDomain)

	return ok && bundle.HasJWTAuthority(keyID)
}

// svidRefusalReason names why go-spiffe refused the token, from its
// unverified claims, for the audit event only.
func svidRefusalReason(token string) string {
	parsed, err := jwt.ParseSigned(token, tokenAlgorithms)
	if err != nil {
		return "invalid SVID"
	}

	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "invalid SVID"
	}

	switch {
	case claims.Expiry == nil || claims.Expiry.Time().Before(time.Now()):
		return "SVID expired"
	case !claims.Audience.Contains(SPIFFEAudience):
		return "SVID audience not toolgate"
	}

	return "invalid SVID"
}

// unverifiedKeyID reads a token's key ID without checking its signature.
func unverifiedKeyID(token string) string {
	parsed, err := jwt.ParseSigned(token, tokenAlgorithms)
	if err != nil || len(parsed.Headers) == 0 {
		return ""
	}

	return parsed.Headers[0].KeyID
}

// bundleAgeCollector exports toolgate_spiffe_bundle_age_seconds: the time
// since each trust domain's last successful bundle fetch.
type bundleAgeCollector struct {
	verifier    *spiffeVerifier
	description *prometheus.Desc
}

func (c bundleAgeCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- c.description
}

func (c bundleAgeCollector) Collect(metrics chan<- prometheus.Metric) {
	for _, source := range c.verifier.sources {
		source.stateMutex.Lock()
		lastSuccess := source.lastSuccess
		source.stateMutex.Unlock()

		if lastSuccess.IsZero() {
			continue
		}

		metrics <- prometheus.MustNewConstMetric(c.description, prometheus.GaugeValue,
			c.verifier.now().Sub(lastSuccess).Seconds(), source.trustDomain.Name())
	}
}

// isSPIFFESubject reports whether a token's unverified subject is a SPIFFE ID.
func isSPIFFESubject(subject string) bool {
	return strings.HasPrefix(subject, spiffeScheme)
}
