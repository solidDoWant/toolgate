package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Envtest is a running kube-apiserver and etcd (controller-runtime's envtest)
// with toolgate's CRDs installed. No controllers run: the API server stores
// and validates objects, but nothing reconciles Deployments or starts pods.
type Envtest struct {
	// Config reaches the API server with cluster-admin rights.
	Config *rest.Config
	// Scheme holds the client-go types and the toolgate types.
	Scheme *k8sruntime.Scheme
	// Client is a controller-runtime client built from Config and Scheme. It
	// reads from the API server directly, without a cache.
	Client client.Client
}

// crdDirectory returns config/crd/bases, the CRD files that `make generate`
// writes and the chart installs.
func crdDirectory(tb testing.TB) string {
	tb.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(tb, ok, "cannot find the testutil source directory")

	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases")
}

// StartEnvtest starts an API server and etcd from the binaries in
// KUBEBUILDER_ASSETS, installs the toolgate CRDs and waits until they are
// served. It skips the test when KUBEBUILDER_ASSETS is unset (the dev shell
// sets it) and stops the API server and etcd when the test ends.
func StartEnvtest(tb testing.TB) *Envtest {
	tb.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		tb.Skip("envtest needs kube-apiserver and etcd, and KUBEBUILDER_ASSETS is not set; run the tests in the dev shell")
	}

	scheme := k8sruntime.NewScheme()
	require.NoError(tb, clientgoscheme.AddToScheme(scheme))
	require.NoError(tb, v1alpha1.AddToScheme(scheme))

	environment := &envtest.Environment{
		Scheme: scheme,
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths:              []string{crdDirectory(tb)},
			ErrorIfPathMissing: true,
		},
	}

	config, err := environment.Start()
	require.NoError(tb, err, "start envtest")
	tb.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			tb.Errorf("stop envtest: %v", err)
		}
	})

	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(tb, err)

	return &Envtest{Config: config, Scheme: scheme, Client: kubeClient}
}
