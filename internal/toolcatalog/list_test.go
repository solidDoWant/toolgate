package toolcatalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

func catalogNames(t *testing.T, template *v1alpha1.MCPServerTemplate, learned []toolcatalog.Learned) ([]string, bool) {
	t.Helper()

	tools, found, err := toolcatalog.CatalogList(template, learned)
	require.NoError(t, err)

	var result []string
	for _, tool := range tools {
		result = append(result, tool.Name)
	}

	return result, found
}

func TestCatalogListOrder(t *testing.T) {
	learned := []toolcatalog.Learned{
		{Generation: 1, Tools: []byte(`[{"name":"old"}]`)},
		{Generation: 3, Tools: []byte(`[{"name":"newer-than-template"}]`)},
		{Generation: 2, Tools: []byte(`[{"name":"mid"}]`)},
	}
	at := func(generation int64, declared ...string) *v1alpha1.MCPServerTemplate {
		result := template(func(spec *v1alpha1.MCPServerTemplateSpec) {
			for _, name := range declared {
				spec.DeclaredTools = append(spec.DeclaredTools, v1alpha1.DeclaredTool{
					Name: name, InputSchema: &runtime.RawExtension{Raw: []byte(`{"type":"object"}`)},
				})
			}
		})
		result.Generation = generation

		return result
	}

	// The generation changes and no row exists for it.
	got, found := catalogNames(t, at(4), learned)
	assert.True(t, found)
	assert.Equal(t, []string{"newer-than-template"}, got, "the newest older generation")

	got, _ = catalogNames(t, at(2), learned)
	assert.Equal(t, []string{"mid"}, got, "the current generation first")

	got, _ = catalogNames(t, at(2), learned[:1])
	assert.Equal(t, []string{"old"}, got, "falls back to an older row")

	got, found = catalogNames(t, at(0), learned[:1])
	assert.False(t, found, "a row of a newer generation isn't used")
	assert.Empty(t, got)

	// Declared tools come first.
	got, found = catalogNames(t, at(2, "declared"), learned)
	assert.True(t, found)
	assert.Equal(t, []string{"declared"}, got)

	_, found = catalogNames(t, at(1), nil)
	assert.False(t, found, "no catalog list")
}
