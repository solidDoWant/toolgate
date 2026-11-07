//go:build integration

package ui_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

func TestSessionsPageShowsPeopleInsteadOfSubjects(t *testing.T) {
	env := startUIEnv(t, "admins")
	admin := env.insertSession(t, "admin-session", []string{"admins"})
	hashed := "914009fed7908eb8fcb6d9435a3aa8d878cf45ec4470a94c688a25539b0125fb"

	env.insertGatewaySession(t, hashed, "known", false)
	env.insertGatewaySession(t, "unknown-subject-0123456789", "unknown", false)
	env.insertGatewaySession(t, "dave", "own", false)

	require.NoError(t, db.New(env.pool).UpsertPrincipalProfile(t.Context(), db.UpsertPrincipalProfileParams{
		Principal: hashed, Email: "fred@example.com", Name: "Fred",
	}))

	response := get(t, admin, env.server.URL+"/ui/sessions")
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, ">Fred &lt;fred@example.com&gt;</span>")
	assert.Contains(t, page, ">user unknown-…</span>")
	assert.Contains(t, page, ">you</span>")
	assert.NotContains(t, page, ">"+hashed+"<")
}

func TestSignInRecordsTheNewestDisplayFields(t *testing.T) {
	env := startUIEnv(t, "")

	signIn := func(user toolgatetest.OIDCUser) {
		t.Helper()

		browser := env.browser(t)
		require.Equal(t, http.StatusSeeOther, env.signIn(t, browser, ui.LoginPath, user).StatusCode)
	}

	profileOf := func() db.PrincipalProfile {
		t.Helper()

		rows, err := db.New(env.pool).ListPrincipalProfiles(t.Context(), []string{"alice"})
		require.NoError(t, err)
		require.Len(t, rows, 1)

		return rows[0]
	}

	signIn(alice)
	assert.Equal(t, "alice@example.com", profileOf().Email)

	signIn(toolgatetest.OIDCUser{Subject: "alice", Email: "alice@example.org", EmailVerified: true})
	assert.Equal(t, "alice@example.org", profileOf().Email, "the newest verified email wins")

	signIn(toolgatetest.OIDCUser{Subject: "bob", Email: "bob@example.com", EmailVerified: false})

	rows, err := db.New(env.pool).ListPrincipalProfiles(t.Context(), []string{"bob"})
	require.NoError(t, err)
	assert.Empty(t, rows, "an email the IdP didn't verify is not stored")
}
