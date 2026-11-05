//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

const (
	releaseName = "toolgate"
	// upgradedCRD is the CRD the upgrade test changes, and upgradeMarker the
	// spec field it adds.
	upgradedCRD   = "settingsprofiles.toolgate.soliddowant.dev"
	upgradedFile  = "toolgate.soliddowant.dev_settingsprofiles.yaml"
	upgradeMarker = "chartUpgradeMarker"
)

var chartDir = filepath.Join("..", "..", "deploy", "charts", "toolgate")

// helmCommand runs helm against the cluster in KUBECONFIG and returns its
// standard output.
func helmCommand(t *testing.T, args ...string) []byte {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}

	var stdout, stderr bytes.Buffer

	command := exec.CommandContext(t.Context(), helm, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Run(), "helm %v: %s", args, stderr.String())

	return stdout.Bytes()
}

// changedChart copies the chart, with its CRDs and library chart, and adds
// an optional spec field to one CRD.
func changedChart(t *testing.T) string {
	t.Helper()

	copied := filepath.Join(t.TempDir(), "toolgate")
	output, err := exec.CommandContext(t.Context(), "cp", "-rL", chartDir, copied).CombinedOutput()
	require.NoError(t, err, string(output))

	path := filepath.Join(copied, "rawCRDs", upgradedFile)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))

	for index := range crd.Spec.Versions {
		spec := crd.Spec.Versions[index].Schema.OpenAPIV3Schema.Properties["spec"]
		spec.Properties[upgradeMarker] = apiextensionsv1.JSONSchemaProps{Type: "string"}
		crd.Spec.Versions[index].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	}

	raw, err = yaml.Marshal(&crd)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	return copied
}

func TestChartUpgradeAndUninstall(t *testing.T) {
	config := adminConfig(t)
	admin := adminClient(t)
	extensions, err := apiextensionsclient.NewForConfig(config)
	require.NoError(t, err)

	markerPresent := func(collect *assert.CollectT) bool {
		crd, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(t.Context(), upgradedCRD, metav1.GetOptions{})
		require.NoError(collect, err)

		for _, version := range crd.Spec.Versions {
			if _, found := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties[upgradeMarker]; !found {
				return false
			}
		}

		return true
	}

	values := helmCommand(t, "get", "values", releaseName, "--namespace", releaseNamespace(), "--output", "yaml")
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(valuesFile, values, 0o600))

	install := func(chart string) {
		helmCommand(t, "upgrade", "--install", releaseName, chart, "--namespace", releaseNamespace(),
			"--values", valuesFile, "--wait", "--timeout", "5m")
	}

	// Whatever happens, leave the release as the harness installed it.
	t.Cleanup(func() { install(chartDir) })

	t.Run("an upgrade applies the changed CRD schema", func(t *testing.T) {
		install(changedChart(t))

		assert.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.True(collect, markerPresent(collect), "the upgraded CRD has spec.%s", upgradeMarker)
		}, time.Minute, time.Second)

		install(chartDir)

		assert.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.False(collect, markerPresent(collect), "the downgraded CRD lost spec.%s", upgradeMarker)
		}, time.Minute, time.Second)
	})

	t.Run("an uninstall keeps the CRDs and the objects using them", func(t *testing.T) {
		namespace, err := admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "chart-uninstall-"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = admin.CoreV1().Namespaces().Delete(t.Context(), namespace.Name, metav1.DeleteOptions{})
		})

		client, err := dynamic.NewForConfig(config)
		require.NoError(t, err)

		profiles := client.Resource(schema.GroupVersionResource{
			Group: "toolgate.soliddowant.dev", Version: "v1alpha1", Resource: "settingsprofiles",
		}).Namespace(namespace.Name)

		profile := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "toolgate.soliddowant.dev/v1alpha1",
			"kind":       "SettingsProfile",
			"metadata":   map[string]any{"name": "kept"},
			"spec":       map[string]any{"settings": map[string]any{}},
		}}
		_, err = profiles.Create(t.Context(), profile, metav1.CreateOptions{})
		require.NoError(t, err)

		helmCommand(t, "uninstall", releaseName, "--namespace", releaseNamespace(), "--wait", "--timeout", "5m")

		for _, name := range []string{
			"mcpservertemplates.toolgate.soliddowant.dev",
			"toolgaterules.toolgate.soliddowant.dev",
			"settingsprofiles.toolgate.soliddowant.dev",
		} {
			crd, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(t.Context(), name, metav1.GetOptions{})
			if assert.NoError(t, err, name) {
				assert.Nil(t, crd.DeletionTimestamp, "%s is not being deleted", name)
			}
		}

		_, err = profiles.Get(t.Context(), "kept", metav1.GetOptions{})
		assert.NoError(t, err, "the SettingsProfile remains")

		install(chartDir)
	})
}
