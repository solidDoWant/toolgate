//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

// identityProvider is the fake OIDC provider the gateway under test trusts.
// It is nil when the harness gave no address (no cluster, or a cluster not
// made by scripts/e2e/run.sh).
var identityProvider *testutil.OIDCProvider

// TestMain starts the fake identity provider on the address and issuer URL
// scripts/e2e/run.sh configured the chart with, so tokens it issues are the
// ones the gateway accepts. All tests of the package share it.
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

		identityProvider, err = testutil.NewOIDCProvider(testutil.OIDCOptions{Listener: listener, Issuer: issuer})
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: fake identity provider: %v\n", err)

			return 1
		}

		defer identityProvider.Close()
	}

	return m.Run()
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
