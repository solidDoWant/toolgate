// Package tokenexchange checks TokenExchange resources: whether the gateway
// may send a user's token to an exchange's token endpoint, and whether the
// rest of the resource can work.
package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// AllowList says which token endpoints exchanges may send tokens to.
type AllowList interface {
	// Allowed reports whether tokenURL is on the list. An error means the
	// list itself could not be read, for example because an issuer's
	// discovery document is unreachable.
	Allowed(ctx context.Context, tokenURL string) (bool, error)
}

// NormalizeURL returns rawURL with its scheme and host in lower case, the
// form allow-list entries and token URLs are compared in. Nothing else
// changes: a path differing only in case or a trailing slash is another URL.
func NormalizeURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)

	return parsed.String(), nil
}

// StaticAllowList is an allow-list the chart sets explicitly.
type StaticAllowList []string

// Allowed implements AllowList with an exact comparison after
// normalization, never a prefix match.
func (s StaticAllowList) Allowed(_ context.Context, tokenURL string) (bool, error) {
	return containsURL(s, tokenURL), nil
}

func containsURL(list []string, tokenURL string) bool {
	normalized, err := NormalizeURL(tokenURL)
	if err != nil {
		return false
	}

	return slices.ContainsFunc(list, func(entry string) bool {
		allowed, err := NormalizeURL(entry)

		return err == nil && allowed == normalized
	})
}

// DiscoveredAllowList is the default allow-list: the token endpoint of each
// user OIDC issuer, read from its discovery document. An issuer's endpoint
// is read once, on first use, and kept; a failed read is tried again on the
// next call.
type DiscoveredAllowList struct {
	// Issuers are the issuer URLs users sign in with.
	Issuers []string
	// Discover returns an issuer's token endpoint. Nil means OIDC discovery.
	Discover func(ctx context.Context, issuerURL string) (string, error)

	mutex     sync.Mutex
	endpoints map[string]string
}

// Allowed implements AllowList.
func (d *DiscoveredAllowList) Allowed(ctx context.Context, tokenURL string) (bool, error) {
	// An issuer that is down doesn't block exchanges with the others.
	endpoints, err := d.tokenEndpoints(ctx)
	if containsURL(endpoints, tokenURL) {
		return true, nil
	}

	return false, err
}

func (d *DiscoveredAllowList) tokenEndpoints(ctx context.Context) ([]string, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	if d.endpoints == nil {
		d.endpoints = make(map[string]string, len(d.Issuers))
	}

	discover := d.Discover
	if discover == nil {
		discover = discoverTokenEndpoint
	}

	var problems []error

	endpoints := make([]string, 0, len(d.Issuers))

	for _, issuer := range d.Issuers {
		endpoint, found := d.endpoints[issuer]
		if !found {
			discovered, err := discover(ctx, issuer)
			if err != nil {
				problems = append(problems, fmt.Errorf("reading the token endpoint of issuer %s: %w", issuer, err))

				continue
			}

			endpoint = discovered
			d.endpoints[issuer] = endpoint
		}

		endpoints = append(endpoints, endpoint)
	}

	return endpoints, errors.Join(problems...)
}

// discoveryTimeout bounds one read of a discovery document.
const discoveryTimeout = 10 * time.Second

func discoverTokenEndpoint(ctx context.Context, issuerURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return "", err
	}

	endpoint := provider.Endpoint().TokenURL
	if endpoint == "" {
		return "", errors.New("the discovery document has no token_endpoint")
	}

	return endpoint, nil
}
