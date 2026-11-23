package main

import (
	"path/filepath"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

// kubeRESTConfig returns the Kubernetes client configuration. A KUBECONFIG
// from getenv wins; otherwise it falls back to the in-cluster configuration
// and the default kubeconfig location.
func kubeRESTConfig(getenv func(string) string) (*rest.Config, error) {
	if paths := getenv("KUBECONFIG"); paths != "" {
		rules := &clientcmd.ClientConfigLoadingRules{Precedence: filepath.SplitList(paths)}

		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}

	return ctrl.GetConfig()
}
