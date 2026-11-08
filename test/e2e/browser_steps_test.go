//go:build e2e

package e2e

import (
	"context"
	"time"

	"github.com/chromedp/chromedp"
)

// browserStepTimeout bounds each browser step. chromedp waits for a selector
// until its context ends, so without a deadline a selector the page no
// longer has would hang the test until go test's own timeout.
const browserStepTimeout = 2 * time.Minute

// browserDo runs steps in the browser under browserStepTimeout. The deadline
// is on a child context: ending it fails the steps but keeps the tab open.
func browserDo(browser context.Context, steps ...chromedp.Action[chromedp.Void]) error {
	ctx, cancel := context.WithTimeout(browser, browserStepTimeout)
	defer cancel()

	return chromedp.Do(ctx, steps...)
}

// browserRun runs action in the browser under browserStepTimeout and returns
// its result.
func browserRun[T any](browser context.Context, action chromedp.Action[T]) (T, error) {
	ctx, cancel := context.WithTimeout(browser, browserStepTimeout)
	defer cancel()

	return chromedp.Run(ctx, action)
}
