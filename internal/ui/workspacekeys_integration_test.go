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

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
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

// activateKeyOf registers a key named name for principal and approves it.
func (e *uiEnv) activateKeyOf(t *testing.T, principal, name string) {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	identity := auth.Identity{Principal: principal, Username: principal, Kind: auth.KindUser, Attestation: auth.AttestationUser}

	registration, err := e.workspaces.RegisterKey(t.Context(), identity, name, base64.RawURLEncoding.EncodeToString(public))
	require.NoError(t, err)

	require.NoError(t, pgx.BeginFunc(t.Context(), e.pool, func(tx pgx.Tx) error {
		return e.workspaces.ApproveKeyRequest(t.Context(), tx, identity, registration.Request)
	}))
}

func (e *uiEnv) keyStatuses(t *testing.T, principal, name string) []string {
	t.Helper()

	rows, err := e.pool.Query(t.Context(), `SELECT status FROM workspace_keys WHERE principal = $1 AND name = $2 ORDER BY status`, principal, name)
	require.NoError(t, err)

	statuses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)

	return statuses
}

func TestWorkspaceKeysCard(t *testing.T) {
	env := startUIEnv(t, "admins")

	card := func(t *testing.T, cookie string) (int, string) {
		t.Helper()

		response := get(t, env.insertSession(t, cookie, []string{"dev"}), env.server.URL+ui.WorkspacesPath)

		return response.StatusCode, readBody(t, response)
	}

	t.Run("keys off and none registered hides the card", func(t *testing.T) {
		env.workspaces.SetWorkspaceKeys(false, "")

		status, body := card(t, "card-hidden")
		require.Equal(t, http.StatusOK, status)
		assert.NotContains(t, body, "Workspace keys")
	})

	t.Run("keys on and none registered shows the empty state", func(t *testing.T) {
		env.workspaces.SetWorkspaceKeys(true, "")

		status, body := card(t, "card-empty")
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, "You have no workspace keys. To add one, run")
		assert.Contains(t, body, "toolgate workspace register NAME")
	})

	t.Run("each user sees only their own keys", func(t *testing.T) {
		env.workspaces.SetWorkspaceKeys(true, "")
		env.activateKeyOf(t, "dave", "mine")
		env.activateKeyOf(t, "erin", "hers")
		pending := env.requestKey(t, "waiting")

		status, body := card(t, "card-own")
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, "registered/dave/mine")
		assert.Contains(t, body, "registered/dave/waiting")
		assert.Regexp(t, `>SHA256:[A-Za-z0-9_-]{12}…</span>`, body)
		assert.Contains(t, body, "Waiting for approval")
		assert.Contains(t, body, ui.ApprovalPath(pending))
		assert.Contains(t, body, "Revoke key mine? Sessions that use it lose their verified workspace on their next request.")
		assert.NotContains(t, body, "registered/erin/hers")

		response := get(t, env.insertSessionOf(t, "erin", "card-erin", []string{"dev"}), env.server.URL+ui.WorkspacesPath)
		erin := readBody(t, response)
		assert.Contains(t, erin, "registered/erin/hers")
		assert.NotContains(t, erin, "registered/dave/mine")
	})

	t.Run("keys off with an active key shows the notice and still lists it", func(t *testing.T) {
		env.workspaces.SetWorkspaceKeys(false, "")

		status, body := card(t, "card-off")
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, "Workspace keys are off on this gateway. These keys don't attest.")
		assert.Contains(t, body, "registered/dave/mine")
	})
}

func TestWorkspaceKeyRevokeForm(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.workspaces.SetWorkspaceKeys(true, "")

	t.Run("revokes the key without a step-up", func(t *testing.T) {
		env.activateKeyOf(t, "dave", "revoke-me")
		pending := env.requestKey(t, "revoke-me")
		browser := env.insertSession(t, "revoke-session", []string{"dev"})
		env.ageSignIn(t, "revoke-session", time.Hour)

		response := post(t, browser, env.server.URL+ui.WorkspaceKeyRevokePath, url.Values{ui.CSRFField: {csrfTokenOf("revoke-session")}, "name": {"revoke-me"}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.WorkspacesPath, location(t, response).Path)
		assert.Equal(t, []string{"revoked"}, env.keyStatuses(t, "dave", "revoke-me"))
		assert.Equal(t, "denied", env.accessRequestStatus(t, pending))

		var via string

		require.NoError(t, env.pool.QueryRow(t.Context(),
			`SELECT details->>'via' FROM audit_events WHERE type = 'workspace.key.revoke' AND principal = 'dave' ORDER BY id DESC LIMIT 1`).Scan(&via))
		assert.Equal(t, "ui", via)
	})

	t.Run("a user cannot revoke another user's key", func(t *testing.T) {
		env.activateKeyOf(t, "erin", "not-yours")
		browser := env.insertSession(t, "revoke-other", []string{"dev"})

		response := post(t, browser, env.server.URL+ui.WorkspaceKeyRevokePath, url.Values{ui.CSRFField: {csrfTokenOf("revoke-other")}, "name": {"not-yours"}})
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
		assert.Equal(t, []string{"active"}, env.keyStatuses(t, "erin", "not-yours"))
	})

	t.Run("refuses a request without a session", func(t *testing.T) {
		env.activateKeyOf(t, "dave", "no-session")

		response := post(t, env.browser(t), env.server.URL+ui.WorkspaceKeyRevokePath, url.Values{"name": {"no-session"}})
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, []string{"active"}, env.keyStatuses(t, "dave", "no-session"))
	})

	t.Run("refuses a request without a CSRF token", func(t *testing.T) {
		env.activateKeyOf(t, "dave", "no-csrf")
		browser := env.insertSession(t, "revoke-csrf", []string{"dev"})

		response := post(t, browser, env.server.URL+ui.WorkspaceKeyRevokePath, url.Values{"name": {"no-csrf"}})
		assert.Equal(t, http.StatusForbidden, response.StatusCode)
		assert.Equal(t, []string{"active"}, env.keyStatuses(t, "dave", "no-csrf"))
	})
}

func TestKeyRequestPages(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.workspaces.SetWorkspaceKeys(true, "custom")
	browser := env.insertSession(t, "key-page", []string{"dev"})
	env.freshSignIn(t, "key-page")

	t.Run("pending request shows its details and the decision buttons", func(t *testing.T) {
		env.activateKeyOf(t, "dave", "page")
		id := env.requestKey(t, "page")

		list := readBody(t, get(t, browser, env.server.URL+ui.ApprovalsPath))
		assert.Regexp(t, `Register workspace key page \(SHA256:[A-Za-z0-9_-]{12}…\)`, list)

		response := get(t, browser, env.server.URL+ui.ApprovalPath(id))
		require.Equal(t, http.StatusOK, response.StatusCode)

		body := readBody(t, response)
		assert.Contains(t, body, "custom/dave/page")
		assert.Regexp(t, `SHA256:[A-Za-z0-9_-]{43}`, body)
		assert.Contains(t, body, "It replaces the key with fingerprint")
		assert.Contains(t, body, "Approve only if you ran")
		assert.Contains(t, body, "unverified")
		assert.Contains(t, body, "/approve")
		assert.Contains(t, body, "/deny")
	})

	t.Run("a new name says no key has it yet", func(t *testing.T) {
		id := env.requestKey(t, "brand-new")

		body := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, body, "No key has this name yet.")
	})

	t.Run("a decided request shows its status only", func(t *testing.T) {
		id := env.requestKey(t, "decided")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/deny", url.Values{ui.CSRFField: {csrfTokenOf("key-page")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		body := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, body, "This request was denied.")
		assert.NotContains(t, body, "/approve")
		assert.NotContains(t, body, "/deny")
	})
}
