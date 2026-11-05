package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

func TestLogoutRemovesTokenFromKeychainAndFile(t *testing.T) {
	keyring.MockInit()

	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", configDir)
	t.Setenv("AppData", configDir)

	userConfigDir, err := os.UserConfigDir()
	require.NoError(t, err)

	tokenDir := filepath.Join(userConfigDir, tokenstore.DirName)
	require.NoError(t, os.MkdirAll(tokenDir, 0o700))
	tokenFile := filepath.Join(tokenDir, tokenstore.FileName)
	require.NoError(t, os.WriteFile(tokenFile, []byte("in-file"), 0o600))
	require.NoError(t, keyring.Set(tokenstore.KeychainService, tokenstore.KeychainUser, "in-keychain"))

	var stdout, stderr bytes.Buffer

	code := run([]string{"logout"}, &stdout, &stderr)

	require.Equal(t, 0, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "Signed out.")

	_, err = keyring.Get(tokenstore.KeychainService, tokenstore.KeychainUser)
	require.ErrorIs(t, err, keyring.ErrNotFound)
	assert.NoFileExists(t, tokenFile)
}
