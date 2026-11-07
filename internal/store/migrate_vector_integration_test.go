//go:build integration

package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestMigrateNamesSearchPathWhenVectorIsNotVisible(t *testing.T) {
	pg := testutil.StartPostgresEmpty(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	_, err := pool.Exec(ctx, "CREATE SCHEMA elsewhere")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "CREATE EXTENSION vector SCHEMA elsewhere")
	require.NoError(t, err)

	err = store.Migrate(ctx, pool)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "search_path")
	assert.Contains(t, err.Error(), `"elsewhere"`)
}
