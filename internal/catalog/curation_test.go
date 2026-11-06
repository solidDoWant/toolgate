package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/catalog"
)

// Curation follows the generation calls use: the current one while valid,
// the last valid one while the current one is invalid, and none for a server
// with no valid generation or one that counts as deleted
// (docs/design/phase-4.md §6, row 246).
func TestCatalogCurationUsesLastValidGenerationWhileInvalid(t *testing.T) {
	templates := catalog.New()

	valid := newTemplate("github", catalog.ScopePerUser, serviceResource)
	valid.Generation = 1
	valid.Spec.Description = "generation 1"
	templates.Set(valid, nil)

	curation, found := templates.Curation("github")
	require.True(t, found)
	assert.Equal(t, "generation 1", curation.Spec.Description)

	invalid := valid.DeepCopy()
	invalid.Generation = 2
	invalid.Spec.Description = "generation 2"
	templates.Set(invalid, []string{"broken"})

	curation, found = templates.Curation("github")
	require.True(t, found)
	assert.Equal(t, "generation 1", curation.Spec.Description)

	neverValid := newTemplate("gitlab", catalog.ScopePerUser, serviceResource)
	templates.Set(neverValid, []string{"broken"})

	_, found = templates.Curation("gitlab")
	assert.False(t, found)

	_, found = templates.Curation("missing")
	assert.False(t, found)

	templates.Delete(keyOf(invalid))

	_, found = templates.Curation("github")
	assert.False(t, found)
}
