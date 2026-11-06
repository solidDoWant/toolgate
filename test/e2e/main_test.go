//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// identityProvider is the fake OIDC provider the gateway under test trusts.
// It is nil when the harness gave no address (no cluster, or a cluster not
// made by scripts/e2e/run.sh).
var identityProvider *testutil.OIDCProvider

// traceReceiver collects the spans the gateway under test exports. It is
// nil when the harness gave no address.
var traceReceiver *testutil.OTLPReceiver

// TestMain starts the fake identity provider on the address and issuer URL
// scripts/e2e/run.sh configured the chart with, so tokens it issues are the
// ones the gateway accepts, and the test OTLP receiver the chart's
// otlpEndpoint points at. All tests of the package share them.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	listen, issuer := os.Getenv("TOOLGATE_E2E_OIDC_LISTEN"), os.Getenv("TOOLGATE_E2E_OIDC_ISSUER")
	if listen != "" && issuer != "" {
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: fake identity provider: %v\n", err)

			return 1
		}

		onSubject, closeDatabase, err := userScopeDefaults(os.Getenv("TOOLGATE_E2E_DATABASE_URL"))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: database: %v\n", err)

			return 1
		}

		defer closeDatabase()

		identityProvider, err = testutil.NewOIDCProvider(testutil.OIDCOptions{
			Listener: listener, Issuer: issuer, OnSubject: onSubject,
			// The gateway's web UI, behind the test's TLS proxy, and the
			// browser harness's direct port-forward.
			UIRedirectURIs: []string{"https://127.0.0.1:*/ui/auth/callback", testutil.DefaultOIDCUIRedirectURI},
		})
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: fake identity provider: %v\n", err)

			return 1
		}

		defer identityProvider.Close()
	}

	if listen := os.Getenv("TOOLGATE_E2E_OTLP_LISTEN"); listen != "" {
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: OTLP receiver: %v\n", err)

			return 1
		}

		traceReceiver = &testutil.OTLPReceiver{ForwardTo: os.Getenv("TOOLGATE_E2E_OTLP_FORWARD_TO")}
		server := &http.Server{Handler: traceReceiver, ReadHeaderTimeout: 10 * time.Second}

		go func() { _ = server.Serve(listener) }()

		defer func() { _ = server.Close() }()
	}

	return m.Run()
}

// userScopeDefaults returns the identity provider hook that gives every
// principal it issues a token for a user_scopes row with both defaults all,
// so the workspace scoping step lets earlier tests see every server. A row
// that already exists is left as it is, so a test can set its own before
// its principal first signs in. With no databaseURL the hook does nothing.
func userScopeDefaults(databaseURL string) (func(context.Context, string) error, func(), error) {
	if databaseURL == "" {
		return nil, func() {}, nil
	}

	pool, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		return nil, nil, err
	}

	return func(ctx context.Context, subject string) error {
		return store.EnsureUserScope(ctx, pool, subject)
	}, pool.Close, nil
}

// otlpReceiver returns the test OTLP receiver, or skips the test when the
// harness started none.
func otlpReceiver(t *testing.T) *testutil.OTLPReceiver {
	t.Helper()

	if traceReceiver == nil {
		t.Skip("TOOLGATE_E2E_OTLP_LISTEN is not set: no OTLP receiver (run through scripts/e2e/run.sh)")
	}

	return traceReceiver
}

// oidcProvider returns the fake identity provider, or skips the test when
// the harness started none.
func oidcProvider(t *testing.T) *testutil.OIDCProvider {
	t.Helper()

	if identityProvider == nil {
		t.Skip("TOOLGATE_E2E_OIDC_LISTEN is not set: no fake identity provider (run through scripts/e2e/run.sh)")
	}

	return identityProvider
}
