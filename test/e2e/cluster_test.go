//go:build e2e

// Package e2e holds the tests that run against a Kubernetes cluster with the
// chart installed. The cluster is the one in KUBECONFIG; the chart's release
// namespace is TOOLGATE_E2E_NAMESPACE (default toolgate-system), and the
// release is named toolgate. The tests skip when no cluster is configured.
package e2e

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// gatewayServiceAccount is the name of the service account the chart creates
// for a release named toolgate.
const gatewayServiceAccount = "toolgate"

func releaseNamespace() string {
	if namespace := os.Getenv("TOOLGATE_E2E_NAMESPACE"); namespace != "" {
		return namespace
	}

	return "toolgate-system"
}

// adminConfig returns the REST configuration of the cluster in KUBECONFIG, or
// skips the test when there is none.
func adminConfig(t *testing.T) *rest.Config {
	t.Helper()

	path := os.Getenv("KUBECONFIG")
	if path == "" {
		t.Skip("KUBECONFIG is not set: no cluster to test against")
	}

	config, err := clientcmd.BuildConfigFromFlags("", path)
	require.NoError(t, err)

	return config
}

func adminClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	client, err := kubernetes.NewForConfig(adminConfig(t))
	require.NoError(t, err)

	return client
}

// gatewayClient returns a client that acts as the gateway's service account,
// through impersonation by the cluster admin.
func gatewayClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	config := rest.CopyConfig(adminConfig(t))
	config.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + releaseNamespace() + ":" + gatewayServiceAccount,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + releaseNamespace(), "system:authenticated"},
	}

	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	return client
}
