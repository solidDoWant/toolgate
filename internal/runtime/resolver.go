package runtime

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Resolver limits (docs/design/phase-2.md §6).
const (
	// MaxResolverResponseBytes bounds the body a resolver answers with.
	MaxResolverResponseBytes = 1 << 20
	// MinResolvedLifetime is how far in the future a resolved value's
	// expiresAt must be.
	MinResolvedLifetime = 2 * time.Minute
)

// ResolverTokenDirectory holds one directory per resolver with the projected
// service-account token the chart mounts for it.
const ResolverTokenDirectory = "/var/run/secrets/toolgate/resolvers"

// ResolveRequest is the body the gateway sends to a resolver. Workspace and
// Session are set only for a per-session instance.
type ResolveRequest struct {
	Resolver  string           `json:"resolver"`
	Template  ResolveTemplate  `json:"template"`
	Instance  string           `json:"instance"`
	Principal ResolvePrincipal `json:"principal"`
	Workspace string           `json:"workspace,omitempty"`
	Session   string           `json:"session,omitempty"`
}

// ResolveTemplate names the template being rendered.
type ResolveTemplate struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ResolvePrincipal is the principal of the render session's snapshot.
type ResolvePrincipal struct {
	ID     string   `json:"id"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

// Resolved is a value from a resolver.
type Resolved struct {
	Value     string
	ExpiresAt time.Time
}

// ResolverCaller calls the resolver a request names.
type ResolverCaller interface {
	Resolve(ctx context.Context, request ResolveRequest) (Resolved, error)
}

// ResolverEndpoint is one configured resolver.
type ResolverEndpoint struct {
	Name    string
	URL     string
	Timeout time.Duration
	// TokenFile is read for each request, because the kubelet rotates it.
	TokenFile string
	client    *http.Client
}

// HTTPResolvers calls configured resolvers over HTTPS.
type HTTPResolvers struct {
	endpoints map[string]ResolverEndpoint
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// ResolverConfig is the configuration of one resolver.
type ResolverConfig struct {
	Name    string
	URL     string
	Timeout time.Duration
	// CAFile, if set, is a PEM file of the CAs that sign the resolver's
	// certificate. Otherwise the system roots are used.
	CAFile string
	// TokenFile is the token path. Empty means the chart's path under
	// ResolverTokenDirectory.
	TokenFile string
}

// NewHTTPResolvers returns the client of the configured resolvers.
func NewHTTPResolvers(configs []ResolverConfig) (*HTTPResolvers, error) {
	resolvers := &HTTPResolvers{endpoints: map[string]ResolverEndpoint{}}

	for _, config := range configs {
		transport := http.DefaultTransport.(*http.Transport).Clone()

		if config.CAFile != "" {
			pem, err := os.ReadFile(config.CAFile)
			if err != nil {
				return nil, fmt.Errorf("resolver %s: caFile: %w", config.Name, err)
			}

			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("resolver %s: caFile %s has no PEM certificate", config.Name, config.CAFile)
			}

			transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}

		tokenFile := config.TokenFile
		if tokenFile == "" {
			tokenFile = filepath.Join(ResolverTokenDirectory, config.Name, "token")
		}

		resolvers.endpoints[config.Name] = ResolverEndpoint{
			Name:      config.Name,
			URL:       config.URL,
			Timeout:   config.Timeout,
			TokenFile: tokenFile,
			client: &http.Client{
				Transport: transport,
				Timeout:   config.Timeout,
				// Redirects aren't followed: the request carries a token.
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		}
	}

	return resolvers, nil
}

// Names returns the names of the configured resolvers.
func (h *HTTPResolvers) Names() []string {
	names := make([]string, 0, len(h.endpoints))
	for name := range h.endpoints {
		names = append(names, name)
	}

	return names
}

// Resolve implements ResolverCaller. Its errors name the resolver and the
// status or field, never the body or the value.
func (h *HTTPResolvers) Resolve(ctx context.Context, request ResolveRequest) (Resolved, error) {
	endpoint, found := h.endpoints[request.Resolver]
	if !found {
		return Resolved{}, fmt.Errorf("resolver `%s` isn't configured", request.Resolver)
	}

	token, err := os.ReadFile(endpoint.TokenFile)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s`: its token can't be read", endpoint.Name)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s`: %w", endpoint.Name, err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(body))
	if err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s`: the request can't be made", endpoint.Name)
	}

	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := endpoint.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return Resolved{}, ctx.Err()
		}

		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return Resolved{}, fmt.Errorf("resolver `%s` didn't answer within %s", endpoint.Name, endpoint.Timeout)
		}

		return Resolved{}, fmt.Errorf("resolver `%s` couldn't be reached", endpoint.Name)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return Resolved{}, fmt.Errorf("resolver `%s` answered HTTP %d", endpoint.Name, response.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResolverResponseBytes+1))
	if err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s`: its answer couldn't be read", endpoint.Name)
	}

	if len(data) > MaxResolverResponseBytes {
		return Resolved{}, fmt.Errorf("resolver `%s` answered with more than 1 MiB", endpoint.Name)
	}

	var answer struct {
		Value     *string `json:"value"`
		ExpiresAt *string `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s` answered with a body that isn't a JSON object", endpoint.Name)
	}

	if answer.Value == nil || *answer.Value == "" {
		return Resolved{}, fmt.Errorf("resolver `%s` answered without a `value`", endpoint.Name)
	}

	if answer.ExpiresAt == nil {
		return Resolved{}, fmt.Errorf("resolver `%s` answered without an `expiresAt`", endpoint.Name)
	}

	expiresAt, err := time.Parse(time.RFC3339, *answer.ExpiresAt)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolver `%s` answered with an `expiresAt` that isn't RFC 3339", endpoint.Name)
	}

	if expiresAt.Before(h.now().Add(MinResolvedLifetime)) {
		return Resolved{}, fmt.Errorf("resolver `%s` answered with an `expiresAt` less than 2 minutes away", endpoint.Name)
	}

	return Resolved{Value: *answer.Value, ExpiresAt: expiresAt}, nil
}

func (h *HTTPResolvers) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}

	return h.Now()
}
