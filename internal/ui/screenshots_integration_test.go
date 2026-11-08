//go:build integration

package ui_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
)

// screenshotsEnv names the directory TestUIScreenshots writes its PNGs to
// (`make ui-screenshots`). Without it the test skips.
const screenshotsEnv = "TOOLGATE_UI_SCREENSHOTS"

// seedScreenshotData fills the gateway with a little of everything the
// pages show, for dave, an admin.
func seedScreenshotData(t *testing.T, env *uiEnv) {
	t.Helper()

	env.addGrantTemplates()

	now := time.Now()

	env.insertListedSession(t, "dave", "laptop-7f3a", "", "attested", "claude-code", 2, -1, true)
	env.insertListedSession(t, "dave", "ci-runner-12", "", "user", "claude-code", 45, 30, false)
	env.insertApproval(t, approvalID(1), "dave", time.Hour)
	env.insertApproval(t, approvalID(2), "dave", time.Hour)

	for index, event := range []struct{ eventType, tool string }{
		{"tool.call", "search_issues"}, {"tool.call", "create_pull_request"}, {"tool.deny", "delete_repo"},
	} {
		env.insertCallEvent(t, event.eventType, "dave", "laptop-7f3a", "github", event.tool, `{}`, now.Add(-time.Duration(index+1)*7*time.Minute))
	}

	_, err := env.pool.Exec(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, created_at, expires_at) VALUES
		('3f9c2a7e1b5d48c0a6e2f17d9b04c8e1', '{"servers":["github"],"tools":["github__delete_repo"]}', 'Incident 42: repository deletions under review.', 'dave', now() - interval '20 minutes', now() + interval '3 hours'),
		('8a1d4e6f2c3b47a9b5d0e8f1a2c3d4e5', '{"groups":["contractors"],"attestation":"user"}', 'Contractor access paused for the audit.', 'dave', now() - interval '2 hours', now() + interval '2 days')`)
	require.NoError(t, err)

	expires := metav1.NewTime(now.Add(30 * 24 * time.Hour))
	env.rules.rules = []v1alpha1.ToolgateRule{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "release-freeze"},
			Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Tools: []string{"ci__deploy"}}, Reason: "Release freeze.", Expires: &expires},
			Status: v1alpha1.Status{Conditions: []metav1.Condition{
				{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Valid", Message: "The rule is valid."},
			}},
		},
	}
}

// TestUIScreenshots renders every web UI page with seeded data, in the
// light and dark themes, on a desktop and a phone, into PNGs for a person
// to review. It checks nothing else.
func TestUIScreenshots(t *testing.T) {
	directory := os.Getenv(screenshotsEnv)
	if directory == "" {
		t.Skipf("set %s to a directory to write the web UI screenshots (make ui-screenshots)", screenshotsEnv)
	}

	require.NoError(t, os.MkdirAll(directory, 0o755))

	env := startUIEnv(t, "toolgate-admins")
	seedScreenshotData(t, env)
	ctx := openBrowserAsAdmin(t, env)

	pages := []struct {
		name, path string
		// prepare runs after the page loads, for example to open a dialog.
		prepare chromedp.Action[chromedp.Void]
	}{
		{name: "servers", path: "/ui/servers"},
		{name: "sessions", path: "/ui/sessions"},
		{name: "workspaces", path: "/ui/workspaces"},
		{name: "secrets", path: "/ui/secrets"},
		{name: "setup", path: "/ui/setup"},
		{name: "approvals", path: "/ui/approvals"},
		{name: "approval", path: "/ui/approvals/" + approvalID(1)},
		{name: "grants", path: "/ui/grants"},
		{name: "grant-new", path: "/ui/grants/new"},
		{name: "audit", path: "/ui/audit"},
		{name: "blocks", path: "/ui/blocks"},
		{name: "blocks-dialog", path: "/ui/blocks", prepare: chromedp.Click(chromedp.CSS(`main a[data-dialog="new-block"]`))},
		{name: "block-new", path: "/ui/blocks/new"},
	}

	viewports := []struct {
		name          string
		width, height int64
	}{
		{"desktop", 1280, 800},
		{"phone", 390, 844},
	}

	for _, theme := range []string{"light", "dark"} {
		for _, viewport := range viewports {
			require.NoError(t, chromedp.Do(ctx,
				chromedp.EmulateViewport(viewport.width, viewport.height),
				chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
					_, err := chromedp.Call(ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
						Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: theme}},
					})

					return err
				}),
			))

			for _, page := range pages {
				steps := []chromedp.Action[chromedp.Void]{toolgatetest.Navigate(env.server.URL + page.path), chromedp.WaitReady(chromedp.CSS("main"))}
				if page.prepare != nil {
					steps = append(steps, page.prepare)
				}

				require.NoError(t, chromedp.Do(ctx, steps...), page.path)

				picture, err := chromedp.Run(ctx, chromedp.CaptureScreenshot())
				require.NoError(t, err, page.path)

				file := filepath.Join(directory, fmt.Sprintf("%s-%s-%s.png", page.name, theme, viewport.name))
				require.NoError(t, os.WriteFile(file, picture, 0o644))
			}
		}
	}

	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	t.Logf("wrote %d screenshots to %s: %s", len(entries), directory, strings.Join(func() []string {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		return names
	}(), ", "))
}
