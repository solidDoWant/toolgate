//go:build integration

package ui_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// layoutScript returns the bounding boxes of every element in main, the copy
// buttons included, plus the size of the scrollable page.
const layoutScript = `JSON.stringify({
	boxes: Array.from(document.querySelectorAll("main, main *")).map(function (element) {
		var box = element.getBoundingClientRect();
		return [element.tagName, box.x, box.y, box.width, box.height];
	}),
	page: [document.documentElement.scrollWidth, document.documentElement.scrollHeight],
})`

func TestCopyButtonDoesNotMoveThePage(t *testing.T) {
	for _, outcome := range []struct{ name, stub, title string }{
		{"copy still running", `new Promise(function () {})`, "Copy the full ID"},
		{"copy done", `Promise.resolve()`, "Copied"},
		{"copy refused", `Promise.reject(new Error("denied"))`, "Copy failed"},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			env := startUIEnv(t, "toolgate-admins")
			ctx := openBrowserAsAdminFor(t, env, 2*time.Minute)

			env.insertApproval(t, approvalID(1), "dave", time.Hour)

			buttons := `main button[data-copy-text]`
			require.NoError(t, chromedp.Do(ctx,
				chromedp.Navigate(env.server.URL+"/ui/approvals/"+approvalID(1)),
				chromedp.WaitVisible(chromedp.CSS(buttons)),
			))
			_, err := chromedp.Run(ctx, chromedp.Evaluate[any](`Object.defineProperty(navigator, "clipboard", {value: {writeText: function () { return `+outcome.stub+`; }}, configurable: true})`))
			require.NoError(t, err)

			measure := func() map[string]any {
				raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](layoutScript))
				require.NoError(t, err)

				var layout map[string]any
				require.NoError(t, json.Unmarshal([]byte(raw), &layout))

				return layout
			}

			before := measure()

			require.NoError(t, chromedp.Do(ctx, chromedp.Click(chromedp.CSS(buttons))))

			if outcome.title != "Copy the full ID" {
				require.Eventually(t, func() bool {
					title, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector('`+buttons+`').title`))
					return err == nil && title == outcome.title
				}, 10*time.Second, 20*time.Millisecond)
			}

			assert.Equal(t, before, measure(), "the copy button and everything around it keep their place and size")
		})
	}
}
