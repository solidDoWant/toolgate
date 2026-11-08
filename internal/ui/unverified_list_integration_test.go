//go:build integration

package ui_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// In a browser, the user picks "Only these servers" with a
// checkbox per server. Adding servers needs a fresh sign-in; removing one
// applies without it.
func TestUnverifiedAllowListPage(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 3*time.Minute)
	defer cancel()

	user := toolgatetest.OIDCUser{Subject: "ivan", Email: "ivan@example.com", EmailVerified: true, Groups: []string{"dev"}}
	unverified := chromedp.CSS(`input[name="unverified"][value="list"]`)
	checkbox := func(server string) chromedp.CSS {
		return chromedp.CSS(`input[name="unverified_server"][value="` + server + `"]`)
	}
	save := chromedp.CSS(`#defaults button[type="submit"]`)

	stored := func() (string, []string) {
		scopes := env.scopesOf(t, user.Subject)

		return scopes.UnverifiedDefault, scopes.UnverifiedServers
	}

	t.Run("with a fresh sign-in, \"Only these servers\" saves the checked servers", func(t *testing.T) {
		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.WorkspacesPath)))
		require.NoError(t, chromedp.Do(ctx, toolgatetest.SignInToOIDC(user), chromedp.WaitVisible(save)))

		assert.True(t, pageShows(ctx, "Only these servers"))
		assert.False(t, pageShows(ctx, "optin-server"), "the servers show only when \"Only these servers\" is picked")

		require.NoError(t, chromedp.Do(ctx,
			chromedp.Click(unverified),
			chromedp.WaitVisible(checkbox("any-server")),
		))

		for _, shown := range []string{"any-server", "optin-server"} {
			assert.True(t, pageShows(ctx, shown), "the page shows %q", shown)
		}

		require.NoError(t, chromedp.Do(ctx,
			chromedp.Click(checkbox("any-server")),
			chromedp.Click(checkbox("optin-server")),
			chromedp.Click(save),
		))

		assert.Eventually(t, func() bool {
			value, servers := stored()

			return value == gateway.ScopeList && slices.Equal(servers, []string{"any-server", "optin-server"})
		}, time.Minute, 100*time.Millisecond)
	})

	t.Run("without a fresh sign-in, removing a server applies and adding one asks for a sign-in", func(t *testing.T) {
		_, err := env.pool.Exec(t.Context(), "UPDATE ui_sessions SET auth_time = now() - interval '6 minutes' WHERE principal = $1", user.Subject)
		require.NoError(t, err)

		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.WorkspacesPath), chromedp.WaitVisible(save)))
		require.NoError(t, chromedp.Do(ctx, chromedp.Click(checkbox("optin-server")), chromedp.Click(save)))

		assert.Eventually(t, func() bool {
			value, servers := stored()

			return value == gateway.ScopeList && slices.Equal(servers, []string{"any-server"})
		}, time.Minute, 100*time.Millisecond)

		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.WorkspacesPath), chromedp.WaitVisible(save)))
		require.NoError(t, chromedp.Do(ctx, chromedp.Click(checkbox("optin-server")), chromedp.Click(save)))

		// The widening part sends the browser to the step-up sign-in.
		assert.Eventually(t, func() bool {
			location, err := chromedp.Run(ctx, chromedp.Location())

			return err == nil && !strings.HasPrefix(location, env.server.URL+ui.WorkspacesPath)
		}, time.Minute, 100*time.Millisecond)

		_, servers := stored()
		assert.Equal(t, []string{"any-server"}, servers, "the stale widening change adds nothing")
	})
}
