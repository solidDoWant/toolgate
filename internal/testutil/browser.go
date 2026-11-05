package testutil

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// BrowserEnv names the Chromium binary for browser tests. Without it,
// FindBrowser looks for Chromium on PATH, where the dev shell puts it.
const BrowserEnv = "TOOLGATE_CHROMIUM"

// browserNames are the Chromium binaries FindBrowser looks for on PATH.
var browserNames = []string{"chromium", "chromium-browser", "google-chrome", "headless-shell"}

// FindBrowser returns the path of the Chromium binary for browser tests, or
// "" when there is none.
func FindBrowser() string {
	if path := os.Getenv(BrowserEnv); path != "" {
		if resolved, err := exec.LookPath(path); err == nil {
			return resolved
		}

		return ""
	}

	for _, name := range browserNames {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}

	return ""
}

// StartBrowser starts a headless Chromium with an empty profile and returns
// a chromedp context for it. Each call is a new browser, with no cookies.
// The browser stops when tb ends. Without Chromium, the test skips. extra
// adds Chromium options.
func StartBrowser(tb testing.TB, extra ...chromedp.ExecAllocatorOption) context.Context {
	tb.Helper()

	path := FindBrowser()
	if path == "" {
		tb.Skipf("no Chromium for browser tests: set %s or put chromium on PATH (the dev shell has it)", BrowserEnv)
	}

	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		// CI runners and containers don't allow Chromium's sandbox; the
		// browser only opens the test's own pages.
		chromedp.NoSandbox,
	)
	options = append(options, extra...)

	allocator, cancelAllocator := chromedp.NewExecAllocator(context.WithoutCancel(tb.Context()), options...)
	browser, cancelBrowser := chromedp.NewContext(allocator)

	tb.Cleanup(func() {
		cancelBrowser()
		cancelAllocator()
	})

	// Start the browser now, so a broken one fails here. The first run
	// takes no timeout: it would stop the browser.
	if err := chromedp.Do(browser); err != nil {
		tb.Fatalf("start Chromium %s: %v", path, err)
	}

	return browser
}

// SignInToOIDC fills and sends the fake OIDC provider's sign-in form, which
// the browser must show, as user. The provider adds or replaces the user,
// with its email, email_verified and groups, and the browser goes on to the
// client's redirect URI.
func SignInToOIDC(user OIDCUser) chromedp.Action[chromedp.Void] {
	emailVerified := "false"
	if user.EmailVerified {
		emailVerified = "true"
	}

	return chromedp.Steps(
		chromedp.WaitVisible(chromedp.ID("subject")),
		chromedp.SetValue(chromedp.ID("subject"), user.Subject),
		chromedp.SetValue(chromedp.ID("email"), user.Email),
		chromedp.SetValue(chromedp.ID("email_verified"), emailVerified),
		chromedp.SetValue(chromedp.ID("groups"), strings.Join(user.Groups, ",")),
		chromedp.Click(chromedp.ID("sign-in")),
	)
}
