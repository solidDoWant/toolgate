package tokenstore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

func TestLockHoldsTheLockFileForOtherProcesses(t *testing.T) {
	for _, mode := range []tokenstore.Mode{tokenstore.ModeFile, tokenstore.ModeKeychain} {
		t.Run(string(mode), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "toolgate")
			store := tokenstore.NewWithMode(dir, mode)

			unlock, err := store.Lock(t.Context())
			require.NoError(t, err)

			// Another process opens the file itself, as this flock does.
			other := flock.New(filepath.Join(dir, tokenstore.LockFileName))

			t.Cleanup(func() { _ = other.Close() })

			locked, err := other.TryLock()
			require.NoError(t, err)
			assert.False(t, locked, "the lock file is locked while Lock is held")

			unlock()

			locked, err = other.TryLock()
			require.NoError(t, err)
			assert.True(t, locked, "unlocking releases the lock file")
		})
	}
}

func TestLockWaitsUntilTheContextEnds(t *testing.T) {
	dir := t.TempDir()

	other := flock.New(filepath.Join(dir, tokenstore.LockFileName))

	t.Cleanup(func() { _ = other.Close() })

	locked, err := other.TryLock()
	require.NoError(t, err)
	require.True(t, locked)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = tokenstore.New(dir).Lock(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// The failed Lock released its own hold, so a later Lock gets the lock
	// once the other holder lets go.
	require.NoError(t, other.Unlock())

	unlock, err := tokenstore.New(dir).Lock(t.Context())
	require.NoError(t, err)
	unlock()
}
