package v1alpha1_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// crdDir is where `make generate` writes the CRDs; the chart reads them from
// there through its rawCRDs link.
const crdDir = "../../config/crd/bases"

var crdFiles = map[string]string{
	"MCPServerTemplate": "toolgate.soliddowant.dev_mcpservertemplates.yaml",
	"ToolgateRule":      "toolgate.soliddowant.dev_toolgaterules.yaml",
	"SettingsProfile":   "toolgate.soliddowant.dev_settingsprofiles.yaml",
}

// field walks nested maps by key and fails the test when one is missing.
func field(t *testing.T, value any, path ...any) any {
	t.Helper()

	for _, key := range path {
		switch key := key.(type) {
		case string:
			object, ok := value.(map[string]any)
			require.Truef(t, ok, "expected an object at %q", key)
			value, ok = object[key]
			require.Truef(t, ok, "missing key %q", key)
		case int:
			list, ok := value.([]any)
			require.True(t, ok, "expected a list")
			require.Greater(t, len(list), key)
			value = list[key]
		}
	}

	return value
}

func TestCRDsHaveStatusConditions(t *testing.T) {
	for kind, file := range crdFiles {
		t.Run(kind, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(crdDir, file))
			require.NoError(t, err)

			var crd map[string]any
			require.NoError(t, yaml.Unmarshal(data, &crd))

			assert.Equal(t, "toolgate.soliddowant.dev", field(t, crd, "spec", "group"))
			assert.Equal(t, kind, field(t, crd, "spec", "names", "kind"))
			version := field(t, crd, "spec", "versions", 0)
			assert.Equal(t, "v1alpha1", field(t, version, "name"))
			field(t, version, "subresources", "status")

			conditions := field(t, version, "schema", "openAPIV3Schema", "properties", "status", "properties", "conditions")
			assert.Equal(t, "array", field(t, conditions, "type"))

			for _, key := range []string{"type", "status", "reason", "message", "lastTransitionTime"} {
				field(t, conditions, "items", "properties", key)
			}
		})
	}
}

func TestCRDDefaultsServerEnvironmentToOptIn(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir, crdFiles["MCPServerTemplate"]))
	require.NoError(t, err)

	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(data, &crd))

	spec := field(t, crd, "spec", "versions", 0, "schema", "openAPIV3Schema", "properties", "spec", "properties")

	assert.Equal(t, "opt-in", field(t, spec, "access", "properties", "environments", "default"))

	toolEnvironments, ok := field(t, spec, "tools", "additionalProperties", "properties", "environments").(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, toolEnvironments, "default", "a tool's environments inherits the server's value")
}
