//go:build integration

package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestInstallIDIsGeneratedOnceAndStable(t *testing.T) {
	pool := openPool(t, testutil.StartPostgres(t).ConnString())

	first, err := store.InstallID(t.Context(), pool)
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{32}$`, first)

	second, err := store.InstallID(t.Context(), pool)
	require.NoError(t, err)
	assert.Equal(t, first, second, "a second call, as after a restart, returns the stored ID")

	other, err := store.InstallID(t.Context(), openPool(t, testutil.StartPostgres(t).ConnString()))
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "another database gets its own ID")
}
