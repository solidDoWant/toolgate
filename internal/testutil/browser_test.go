package testutil_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestStartBrowserSkipsWithoutChromium(t *testing.T) {
	t.Setenv(testutil.BrowserEnv, "")
	t.Setenv("PATH", t.TempDir())

	require.Empty(t, testutil.FindBrowser())

	var skipped bool

	t.Run("browser test", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()

		testutil.StartBrowser(t)
		t.Error("StartBrowser returned without a browser")
	})

	assert.True(t, skipped, "a browser test skips when there is no Chromium")
}

func TestStartBrowserSkipsWithMissingChromiumPath(t *testing.T) {
	t.Setenv(testutil.BrowserEnv, t.TempDir()+"/no-chromium")

	assert.Empty(t, testutil.FindBrowser())
}

func TestBrowserSignsInThroughOIDCProvider(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{})
	browser := testutil.StartBrowser(t)

	verifier := oauth2.GenerateVerifier()
	user := testutil.OIDCUser{Subject: "bob", Email: "bob@example.com", EmailVerified: true, Groups: []string{"operators"}}

	require.NoError(t, chromedp.Do(browser, testutil.Navigate(flow.config.AuthCodeURL("state-1", oauth2.S256ChallengeOption(verifier)))))

	title, err := chromedp.Run(browser, chromedp.Title())
	require.NoError(t, err)
	assert.Equal(t, "Sign in to the fake identity provider", title)

	require.NoError(t, chromedp.Do(browser, testutil.SignInToOIDC(user)))

	var location string

	require.Eventually(t, func() bool {
		location, err = chromedp.Run(browser, chromedp.Location())

		return err == nil && strings.HasPrefix(location, flow.config.RedirectURL)
	}, 30*time.Second, 100*time.Millisecond, "the browser reaches the callback")

	callback, err := url.Parse(location)
	require.NoError(t, err)

	claims := flow.exchange(t, callback, verifier)
	assert.Equal(t, "bob", claims["sub"])
	assert.Equal(t, []any{"operators"}, claims["groups"])
}
