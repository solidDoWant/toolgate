//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/yaml"

	"github.com/solidDoWant/toolgate/internal/store"
)

// harnessSecrets are the Secrets of the harness's release namespace that an
// extra install mounts or reads; the chart's values name them.
var harnessSecrets = []string{"toolgate-resolver-ca", "toolgate-secrets-keyset"}

// extraScopeDatabases are the pools of the databases of extra installs. The
// fake identity provider gives every principal a row with both defaults all
// in each of them, as it does in the harness's database.
var (
	extraScopeMutex     sync.Mutex
	extraScopeDatabases = map[string]func(context.Context, string) error{}
)

// ensureExtraUserScopes is called by the identity provider's hook for every
// principal.
func ensureExtraUserScopes(ctx context.Context, subject string) error {
	extraScopeMutex.Lock()

	ensures := make([]func(context.Context, string) error, 0, len(extraScopeDatabases))
	for _, ensure := range extraScopeDatabases {
		ensures = append(ensures, ensure)
	}

	extraScopeMutex.Unlock()

	for _, ensure := range ensures {
		if err := ensure(ctx, subject); err != nil {
			return err
		}
	}

	return nil
}

// gatewayInstallOptions change what installGateway installs.
type gatewayInstallOptions struct {
	// Values are chart values that replace the harness's, as a nested map.
	Values map[string]any
	// Embeddings keeps the bundled embeddings server. It is off by default
	// because starting it is the slowest part of an install.
	Embeddings bool
}

// gatewayInstall is a toolgate install that one test owns.
type gatewayInstall struct {
	Namespace string
	// Database is the name of the install's PostgreSQL database.
	Database string
}

// installGateway installs the chart for the calling top-level test, in a new
// namespace with its own PostgreSQL database, and makes releaseNamespace
// return that namespace for the test and its subtests. It shares the
// cluster, Flux, PostgreSQL, S3, the CRDs and the images with the harness
// install, and starts from the harness's chart values, so the install is
// quick. Everything it made is removed when the test ends.
func installGateway(t *testing.T, options gatewayInstallOptions) *gatewayInstall {
	t.Helper()

	top, _, _ := strings.Cut(t.Name(), "/")
	require.Equal(t, t.Name(), top, "installGateway is called from the top-level test")

	admin := adminClient(t)
	harness := harnessNamespace()
	suffix := strings.ToLower(rand.String(6))
	namespace := "e2e-" + suffix
	cleanup := func() context.Context { return context.WithoutCancel(t.Context()) }

	// The database host and the superuser come from the harness's own
	// connection string.
	harnessDatabase, err := admin.CoreV1().Secrets(harness).Get(t.Context(), "toolgate-database", metav1.GetOptions{})
	require.NoError(t, err)

	original, err := url.Parse(string(harnessDatabase.Data["url"]))
	require.NoError(t, err)

	hostParts := strings.Split(original.Hostname(), ".")
	require.GreaterOrEqual(t, len(hostParts), 2, "the host is postgres.<namespace>.svc")

	postgresNamespace, superuser := hostParts[1], original.User.Username()
	database := "inst" + suffix

	output, err := psql(t, postgresNamespace, superuser, "postgres", "CREATE DATABASE "+database)
	require.NoError(t, err, output)

	t.Cleanup(func() {
		_, _ = psql(t, postgresNamespace, superuser, "postgres", "DROP DATABASE "+database+" WITH (FORCE)")
	})

	connection := *original
	connection.Path = "/" + database

	_, err = admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() { _ = admin.CoreV1().Namespaces().Delete(cleanup(), namespace, metav1.DeleteOptions{}) })

	secrets := []*corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Name: "toolgate-database"},
		StringData: map[string]string{"url": connection.String()},
	}}

	for _, name := range harnessSecrets {
		source, err := admin.CoreV1().Secrets(harness).Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)

		secrets = append(secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: source.Data})
	}

	for _, secret := range secrets {
		_, err := admin.CoreV1().Secrets(namespace).Create(t.Context(), secret, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	base := helmCommand(t, "get", "values", releaseName, "--namespace", harness, "--output", "yaml")
	directory := t.TempDir()
	baseFile := filepath.Join(directory, "base.yaml")
	require.NoError(t, os.WriteFile(baseFile, base, 0o600))

	overrides := map[string]any{
		"config": map[string]any{
			"crds":            map[string]any{"enabled": false},
			"sharedNamespace": map[string]any{"name": "shared-" + suffix},
			// The chart names the Role it makes in this namespace after the
			// release, so a second release could not make its own.
			"templateSecretsNamespace": "",
		},
	}
	if !options.Embeddings {
		overrides["embeddings"] = map[string]any{"server": map[string]any{"enabled": false}}
	}

	mergeValues(overrides, options.Values)

	raw, err := yaml.Marshal(overrides)
	require.NoError(t, err)

	overrideFile := filepath.Join(directory, "override.yaml")
	require.NoError(t, os.WriteFile(overrideFile, raw, 0o600))

	ownInstances.Store(top, namespace)
	t.Cleanup(func() { ownInstances.Delete(top) })

	t.Cleanup(func() {
		helmCommand(t, "uninstall", releaseName, "--namespace", namespace, "--wait", "--timeout", "5m")
	})

	helmCommand(t, "install", releaseName, chartDir, "--namespace", namespace,
		"--values", baseFile, "--values", overrideFile, "--wait", "--timeout", "5m")

	// Give every principal a user_scopes row in the new database, as the
	// harness does in its own. The table exists once the gateway has migrated
	// the database, so this comes after the install.
	if databaseURL := os.Getenv("TOOLGATE_E2E_DATABASE_URL"); databaseURL != "" {
		extra, err := url.Parse(databaseURL)
		require.NoError(t, err)

		extra.Path = "/" + database

		pool, err := store.Open(t.Context(), extra.String())
		require.NoError(t, err)

		extraScopeMutex.Lock()
		extraScopeDatabases[namespace] = func(ctx context.Context, subject string) error { return store.EnsureUserScope(ctx, pool, subject) }
		extraScopeMutex.Unlock()

		t.Cleanup(func() {
			extraScopeMutex.Lock()
			delete(extraScopeDatabases, namespace)
			extraScopeMutex.Unlock()
			pool.Close()
		})
	}

	return &gatewayInstall{Namespace: namespace, Database: database}
}

// harnessNamespace is the release namespace of the harness's install.
func harnessNamespace() string {
	if namespace := os.Getenv("TOOLGATE_E2E_NAMESPACE"); namespace != "" {
		return namespace
	}

	return "toolgate-system"
}

// mergeValues copies source into target, merging nested maps.
func mergeValues(target, source map[string]any) {
	for key, value := range source {
		nested, isMap := value.(map[string]any)
		existing, existingIsMap := target[key].(map[string]any)

		if isMap && existingIsMap {
			mergeValues(existing, nested)

			continue
		}

		target[key] = value
	}
}
