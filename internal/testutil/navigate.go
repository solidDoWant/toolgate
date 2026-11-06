package testutil

import (
	"context"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	// networkChangedError is the error Chromium reports when the host's
	// network interfaces change during a navigation, which happens when
	// Docker or kind networks come and go. Chromium has no switch to turn
	// this off.
	networkChangedError = "net::ERR_NETWORK_CHANGED"

	// navigateAttempts is how many times Navigate tries a page.
	navigateAttempts = 3

	// navigateBackoff is the wait before the first retry; it doubles on each
	// further retry.
	navigateBackoff = 200 * time.Millisecond
)

// Navigate is like chromedp.Navigate, but it tries again, at most 3 times in
// all, when Chromium fails with net::ERR_NETWORK_CHANGED. Any other error
// fails at once.
func Navigate(url string) chromedp.Action[chromedp.Void] {
	return func(ctx context.Context, target *chromedp.Target) (chromedp.Void, error) {
		return chromedp.Void{}, retryNetworkChanged(ctx, navigateAttempts, navigateBackoff, func() error {
			_, err := chromedp.Navigate(url)(ctx, target)

			return err
		})
	}
}

// retryNetworkChanged runs attempt up to attempts times, waiting backoff
// (doubling each time) between tries, while it fails with
// net::ERR_NETWORK_CHANGED. It returns the first other result, or the last
// error.
func retryNetworkChanged(ctx context.Context, attempts int, backoff time.Duration, attempt func() error) error {
	var err error

	for try := range attempts {
		if err = attempt(); err == nil || !strings.Contains(err.Error(), networkChangedError) {
			return err
		}

		if try == attempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
			backoff *= 2
		}
	}

	return err
}

// BodyText returns the text content of the page body. It reads the text in
// one script run in the page, so it cannot hit a node that a navigation or a
// re-render replaced between resolving and reading it.
func BodyText() chromedp.Action[string] {
	return chromedp.Evaluate[string](`document.body ? document.body.textContent : ""`)
}
