package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
)

func TestWriteReplacesTheFileInOneStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o644))

	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, atomicfile.Write(path, []byte(`{"new":true}`), 0o644))

	after, err := os.Stat(path)
	require.NoError(t, err)

	// A rename puts a new file in place; writing in place would keep the old
	// one, and a reader could see it half written.
	assert.False(t, os.SameFile(before, after), "the file is replaced by a rename, not rewritten in place")
	assert.Equal(t, os.FileMode(0o644), after.Mode().Perm())

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"new":true}`, string(content))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}

func TestWriteFailureKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.Mkdir(path, 0o700))

	require.Error(t, atomicfile.Write(path, []byte(`{}`), 0o600))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}
