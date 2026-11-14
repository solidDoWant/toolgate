//go:build integration

package ui_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// requestKey registers a new key of dave's named name and returns its
// pending key request.
func (e *uiEnv) requestKey(t *testing.T, name string) string {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	identity := daveIdentity()
	identity.Username = "dave"

	registration, err := e.workspaces.RegisterKey(t.Context(), identity, name, base64.RawURLEncoding.EncodeToString(public))
	require.NoError(t, err)
	require.NotEmpty(t, registration.Request)

	return registration.Request
}

func (e *uiEnv) activeKeys(t *testing.T, name string) int {
	t.Helper()

	var count int

	require.NoError(t, e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM workspace_keys WHERE principal = 'dave' AND name = $1 AND status = 'active'`, name).Scan(&count))

	return count
}

// Only a browser session with a fresh sign-in approves a key request: the
// approval route reads the UI session cookie, never a bearer token.
func TestWorkspaceKeyApproval(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.workspaces.SetWorkspaceKeys(true, "")

	t.Run("bearer token cannot approve", func(t *testing.T) {
		id := env.requestKey(t, "bearer")
		token := env.idp.MintAccessToken(t, toolgatetest.TokenClaims{Subject: "dave", Groups: []string{"dev"}})

		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.server.URL+ui.ApprovalPath(id)+"/approve", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+token)

		response, err := env.browser(t).Do(request)
		require.NoError(t, err)

		_ = response.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
		assert.Zero(t, env.activeKeys(t, "bearer"))
	})

	t.Run("stale sign-in cannot approve", func(t *testing.T) {
		id := env.requestKey(t, "stale")

		browser := env.insertSession(t, "key-stale", []string{"dev"})
		env.freshSignIn(t, "key-stale")
		env.ageSignIn(t, "key-stale", ui.StepUpMaxAge+30*time.Second)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("key-stale")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
		assert.Zero(t, env.activeKeys(t, "stale"))
	})

	t.Run("fresh sign-in approves the key", func(t *testing.T) {
		id := env.requestKey(t, "fresh")

		browser := env.insertSession(t, "key-fresh", []string{"dev"})
		env.freshSignIn(t, "key-fresh")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("key-fresh")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.ApprovalPath(id), location(t, response).Path)
		assert.Equal(t, "approved", env.accessRequestStatus(t, id))
		assert.Equal(t, 1, env.activeKeys(t, "fresh"))
	})
}
