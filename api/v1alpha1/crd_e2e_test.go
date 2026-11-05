//go:build e2e

package v1alpha1_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kindClusterName names the test cluster after the checkout (toolgate-<name>,
// with an "issue-" prefix dropped), so parallel checkouts don't collide.
// TOOLGATE_KIND_CLUSTER overrides it.
func kindClusterName(t *testing.T) string {
	t.Helper()

	if name := os.Getenv("TOOLGATE_KIND_CLUSTER"); name != "" {
		return name
	}

	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	return "toolgate-" + strings.TrimPrefix(filepath.Base(root), "issue-")
}

func run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(stdin)

	var output bytes.Buffer

	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()

	return output.String(), err
}

// kindCluster returns a kubeconfig for a kind cluster with the toolgate CRDs
// applied. It creates the cluster if it doesn't exist, and then deletes it
// when the test ends.
func kindCluster(t *testing.T) string {
	t.Helper()

	for _, tool := range []string{"kind", "kubectl", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}

	if output, err := run(t.Context(), "", "docker", "info"); err != nil {
		t.Skipf("docker is not available: %v: %s", err, output)
	}

	name := kindClusterName(t)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	clusters, err := run(t.Context(), "", "kind", "get", "clusters")
	require.NoError(t, err, clusters)

	if !strings.Contains("\n"+clusters, "\n"+name+"\n") {
		output, err := run(t.Context(), "", "kind", "create", "cluster", "--name", name, "--kubeconfig", kubeconfig, "--wait", "2m")
		require.NoError(t, err, output)
		t.Cleanup(func() {
			output, err := run(context.Background(), "", "kind", "delete", "cluster", "--name", name)
			assert.NoError(t, err, output)
		})
	} else {
		output, err := run(t.Context(), "", "kind", "export", "kubeconfig", "--name", name, "--kubeconfig", kubeconfig)
		require.NoError(t, err, output)
	}

	output, err := run(t.Context(), "", "kubectl", "--kubeconfig", kubeconfig, "apply", "--server-side", "-f", crdDir)
	require.NoError(t, err, output)
	output, err = run(t.Context(), "", "kubectl", "--kubeconfig", kubeconfig, "wait", "--for=condition=Established", "--timeout=60s", "-f", crdDir)
	require.NoError(t, err, output)

	return kubeconfig
}

const validTemplate = `
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata: {name: github, namespace: default}
spec:
  description: GitHub issues, PRs and repositories
  scope: per-session
  namespace: per-instance
  idleStop: 30m
  startTimeout: 5m
  access:
    groups: [dev]
    environments: opt-in
  tools:
    delete_repository: {environments: attested, approval: required}
    list_secrets: {blocked: true}
  endpoint: {service: github-mcp, port: 8080, path: /mcp}
  resources:
    - apiVersion: v1
      kind: Service
      metadata: {name: github-mcp}
      spec:
        ports: [{port: 8080}]
`

func TestCRDValidationRejectsInvalidTemplates(t *testing.T) {
	kubeconfig := kindCluster(t)

	tests := []struct {
		name    string
		replace [2]string
		message string
	}{
		{
			name:    "invalid server name",
			replace: [2]string{"name: github,", "name: GitHub-Server,"},
			message: "the name is the server name and must match ^[a-z][a-z0-9-]{0,30}$",
		},
		{
			name:    "server name over 31 characters",
			replace: [2]string{"name: github,", "name: a1234567890123456789012345678901,"},
			message: "the name is the server name and must match ^[a-z][a-z0-9-]{0,30}$",
		},
		{
			name:    "unknown scope",
			replace: [2]string{"scope: per-session", "scope: per-team"},
			message: "scope must be one of per-session, per-user, shared",
		},
		{
			name:    "access.environments outside any, opt-in, attested",
			replace: [2]string{"environments: opt-in", "environments: sometimes"},
			message: "environments must be one of any, opt-in, attested",
		},
		{
			name:    "shared namespace with a per-session scope",
			replace: [2]string{"namespace: per-instance", "namespace: shared"},
			message: "namespace: shared is allowed only with scope: shared",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := strings.Replace(validTemplate, test.replace[0], test.replace[1], 1)
			require.NotEqual(t, validTemplate, manifest)
			output, err := run(t.Context(), manifest, "kubectl", "--kubeconfig", kubeconfig, "create", "--dry-run=server", "-f", "-")
			require.Error(t, err, output)
			assert.Contains(t, output, test.message)
		})
	}

	t.Run("valid template is accepted", func(t *testing.T) {
		output, err := run(t.Context(), validTemplate, "kubectl", "--kubeconfig", kubeconfig, "create", "--dry-run=server", "-f", "-")
		require.NoError(t, err, output)
	})

	t.Run("31-character server name is accepted", func(t *testing.T) {
		manifest := strings.Replace(validTemplate, "name: github,", "name: a123456789012345678901234567890,", 1)
		output, err := run(t.Context(), manifest, "kubectl", "--kubeconfig", kubeconfig, "create", "--dry-run=server", "-f", "-")
		require.NoError(t, err, output)
	})
}
