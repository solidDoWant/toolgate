package settingssync_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/settingssync"
)

func profileOf(t *testing.T, fragment string) map[string]json.RawMessage {
	t.Helper()

	var profile map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(fragment), &profile))

	return profile
}

func syncPaths(t *testing.T, initial string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "claude", "settings.json")

	if initial != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))
		require.NoError(t, os.WriteFile(settingsPath, []byte(initial), 0o600))
	}

	return settingsPath, filepath.Join(dir, "toolgate", settingssync.OwnedFileName)
}

func fileContent(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(content)
}

func TestSyncLeavesAKeyTheUserSetBeforeTheFirstSync(t *testing.T) {
	settingsPath, ownedPath := syncPaths(t, `{"model":"sonnet","env":{"FOO":"mine"}}`)

	result, err := settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{"model":"opus","env":{"FOO":"1","BAR":"2"}}`))
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"model", "env.FOO"}, result.UserChanged)
	assert.Equal(t, []string{"env.BAR"}, result.Written)
	assert.JSONEq(t, `{"model":"sonnet","env":{"FOO":"mine","BAR":"2"}}`, fileContent(t, settingsPath))
}

func TestSyncComparesValuesAsParsedJSON(t *testing.T) {
	settingsPath, ownedPath := syncPaths(t, `{}`)

	_, err := settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{"permissions":{"allow":["a","b"]},"hooks":{"x":1}}`))
	require.NoError(t, err)

	// The file is reformatted, but holds the same values.
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"permissions": {"allow": [ "a", "b" ]}, "hooks": {"x": 1.0}}`), 0o600))

	result, err := settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{"permissions":{"allow":["c"]},"hooks":{"x":2}}`))
	require.NoError(t, err)

	assert.Empty(t, result.UserChanged)
	assert.JSONEq(t, `{"permissions":{"allow":["c"]},"hooks":{"x":2}}`, fileContent(t, settingsPath))
}

func TestSyncKeepsAUserEditedKeyTheProfileDropped(t *testing.T) {
	settingsPath, ownedPath := syncPaths(t, "")

	_, err := settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{"model":"opus","theme":"dark"}`))
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"model":"sonnet","theme":"dark"}`), 0o600))

	result, err := settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"theme"}, result.Removed)
	assert.Equal(t, []string{"model"}, result.UserChanged)
	assert.JSONEq(t, `{"model":"sonnet"}`, fileContent(t, settingsPath))
	assert.JSONEq(t, `{"keys":[]}`, fileContent(t, ownedPath), "a dropped key is no longer owned either way")

	// The key is no longer owned, so a profile that sets it again doesn't
	// overwrite the user's value.
	result, err = settingssync.Sync(settingsPath, ownedPath, profileOf(t, `{"model":"opus"}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"model"}, result.UserChanged)
	assert.JSONEq(t, `{"model":"sonnet"}`, fileContent(t, settingsPath))
}
