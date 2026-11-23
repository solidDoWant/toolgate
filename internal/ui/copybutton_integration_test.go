//go:build integration

package ui_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
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
	t.Parallel()

	for _, outcome := range []struct{ name, stub, title, state string }{
		{"copy still running", `new Promise(function () {})`, "Copy the full ID", ""},
		{"copy done", `Promise.resolve()`, "Copied", "done"},
		{"copy refused", `Promise.reject(new Error("denied"))`, "Copy failed", "failed"},
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

			// The full ID is on the page as text, not shortened.
			text, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector("main").innerText`))
			require.NoError(t, err)
			assert.Contains(t, text, approvalID(1), "the whole request ID is readable")

			before := measure()

			// The button's centre, for the pointer events.
			centre, err := chromedp.Run(ctx, chromedp.Evaluate[[]float64](`(function () { var b = document.querySelector('`+buttons+`').getBoundingClientRect(); return [b.x + b.width / 2, b.y + b.height / 2]; })()`))
			require.NoError(t, err)

			steps := []struct {
				name string
				do   func() error
			}{
				{"hover", func() error {
					_, err := chromedp.Run(ctx, chromedp.MouseEvent(input.DispatchMouseEventTypeMouseMoved, centre[0], centre[1]))
					return err
				}},
				{"mouse down", func() error {
					_, err := chromedp.Run(ctx, chromedp.MouseEvent(input.DispatchMouseEventTypeMousePressed, centre[0], centre[1], chromedp.ButtonLeft, chromedp.ClickCount(1)))
					return err
				}},
				{"mouse up", func() error {
					_, err := chromedp.Run(ctx, chromedp.MouseEvent(input.DispatchMouseEventTypeMouseReleased, centre[0], centre[1], chromedp.ButtonLeft, chromedp.ClickCount(1)))
					return err
				}},
			}
			for _, step := range steps {
				require.NoError(t, step.do(), step.name)
				assert.Equal(t, before, measure(), "nothing moves after %s", step.name)
			}

			if outcome.state != "" {
				require.Eventually(t, func() bool {
					state, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector('`+buttons+`').getAttribute("data-state") || ""`))
					return err == nil && state == outcome.state
				}, 10*time.Second, 20*time.Millisecond)

				title, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector('`+buttons+`').title`))
				require.NoError(t, err)
				assert.Equal(t, outcome.title, title)

				announced, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector('main [data-copy-status]').textContent`))
				require.NoError(t, err)
				assert.Equal(t, outcome.title, announced, "screen readers hear the result")

				assert.Equal(t, before, measure(), "nothing moves once the clipboard call is done")

				// After the confirmation time the button is idle again.
				require.Eventually(t, func() bool {
					state, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.querySelector('`+buttons+`').getAttribute("data-state") || ""`))
					return err == nil && state == ""
				}, 10*time.Second, 50*time.Millisecond)
				assert.Equal(t, before, measure(), "nothing moves when the confirmation ends")
			}
		})
	}
}
