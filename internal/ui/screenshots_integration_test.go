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
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// screenshotsEnv names the directory TestUIScreenshots writes its PNGs to
// (`make ui-screenshots`). Without it the test skips.
const screenshotsEnv = "TOOLGATE_UI_SCREENSHOTS"

// seedScreenshotSecrets gives dave three secrets: one a server uses and one
// that was replaced, so its Created time differs from its Updated time.
func seedScreenshotSecrets(t *testing.T, env *uiEnv) {
	t.Helper()

	var err error

	for _, secret := range []string{"api-key", "mail-archiver-token", "unused-token"} {
		_, err = env.broker.Set(t.Context(), "dave", secret, []byte("not-shown"))
		require.NoError(t, err)
	}

	_, err = env.pool.Exec(t.Context(), `UPDATE user_secrets SET created_at = now() - interval '9 days', updated_at = now() - interval '3 days' WHERE name = 'mail-archiver-token'`)
	require.NoError(t, err)
}

// seedScreenshotData fills the gateway with a little of everything the
// pages show, for dave, an admin.
func seedScreenshotData(t *testing.T, env *uiEnv) {
	t.Helper()

	env.addGrantTemplates()

	now := time.Now()

	env.insertListedSession(t, "dave", "laptop-7f3a", "", "attested", "claude-code", 2, -1, true)
	env.insertListedSession(t, "dave", "ci-runner-12", "", "user", "claude-code", 45, 30, false)
	require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(),
		gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/mail", Servers: []string{"any-server", "optin-server"}}, gateway.ViaUI))
	require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(),
		gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: []string{"any-server"}}, gateway.ViaUI))
	env.insertApproval(t, approvalID(1), "dave", time.Hour)
	env.insertApproval(t, approvalID(2), "dave", time.Hour)

	_, err := env.pool.Exec(t.Context(), `INSERT INTO grants (id, principal, template_ns, template_name, tool, session_id, created_at, expires_at) VALUES
		('c1d2e3f4a5b60718293a4b5c6d7e8f90', 'dave', 'tools', 'optin-server', '', NULL, now() - interval '10 minutes', now() + interval '50 minutes'),
		('d1d2e3f4a5b60718293a4b5c6d7e8f91', 'dave', 'tools', 'any-server', 'deploy', 'laptop-7f3a', now() - interval '20 minutes', now() + interval '8 hours')`)
	require.NoError(t, err)
	env.insertSession(t, "dev-session", []string{"toolgate-admins", "dev"})
	env.insertSession(t, "stale-session", []string{"toolgate-admins"})
	env.ageSignIn(t, "stale-session", time.Hour)
	env.insertSession(t, "fresh-session", []string{"toolgate-admins"})
	env.ageSignIn(t, "fresh-session", 0)
	env.insertSessionOf(t, "erin", "empty-session", []string{"dev"})

	for index, event := range []struct{ tool, details string }{
		{"search_issues", `{"decision":"allow"}`}, {"create_pull_request", `{"decision":"allow"}`}, {"delete_repo", `{"decision":"refuse","rule":"no-deletes"}`},
	} {
		env.insertCallEvent(t, "tool.decision", "dave", "laptop-7f3a", "github", event.tool, event.details, now.Add(-time.Duration(index+1)*7*time.Minute))
	}

	for index, event := range []struct{ eventType, tool, details string }{
		{"tool.decision", "delete_repo", `{"decision":"refuse","rule":"no-deletes"}`},
		{"tool.result", "search_issues", `{"outcome":"ok","duration_ms":420}`},
		{"tool.decision", "create_pull_request", `{"decision":"allow"}`},
	} {
		env.insertAudit(t, auditRow{
			at: now.Add(-time.Duration(index+1) * 5 * time.Minute), principal: "dave", workspace: "acme/web", session: "laptop-7f3a",
			server: "github", tool: event.tool, eventType: event.eventType, details: event.details,
		})
	}

	_, err = env.pool.Exec(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, created_at, expires_at) VALUES
		('3f9c2a7e1b5d48c0a6e2f17d9b04c8e1', '{"servers":["github"],"tools":["github__delete_repo"]}', 'Incident 42: repository deletions under review.', 'dave', now() - interval '20 minutes', now() + interval '3 hours'),
		('8a1d4e6f2c3b47a9b5d0e8f1a2c3d4e5', '{"groups":["contractors"],"attestation":"user"}', 'Contractor access paused for the audit.', 'dave', now() - interval '2 hours', now() + interval '2 days')`)
	require.NoError(t, err)

	seedScreenshotSecrets(t, env)

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
	ctx := openBrowserAsAdminFor(t, env, 10*time.Minute)

	pages := []struct {
		name, path string
		// prepare runs after the page loads, for example to open a dialog.
		prepare chromedp.Action[chromedp.Void]
		// cookie is the UI session the page opens with: the admin's by
		// default, or one of a user who has nothing, for empty states.
		cookie string
		// servers is the state of the fake catalog: "seeded" unless set.
		servers string
		// before runs before the page loads, to change the seeded data.
		before func()
		// setup changes the seeded data before the page loads; the function
		// it returns restores it.
		setup func() func()
	}{
		{name: "servers", path: "/ui/servers"},
		{name: "servers-empty", path: "/ui/servers", servers: "empty"},
		{name: "server", path: "/ui/servers/github"},
		{name: "server-no-tools", path: "/ui/servers/mail"},
		{name: "sessions", path: "/ui/sessions"},
		{name: "workspaces", path: "/ui/workspaces"},
		{name: "workspaces-choose", path: "/ui/workspaces", prepare: chromedp.Click(chromedp.CSS(`main label.btn-small[for="choose-0"]`))},
		{name: "workspaces-only-these", path: "/ui/workspaces", prepare: chromedp.Click(chromedp.CSS(`main input[name="unverified"][value="list"]`))},
		{name: "workspaces-empty", path: "/ui/workspaces", setup: func() func() {
			change := gateway.WorkspaceChange{Action: gateway.WorkspaceClear, Workspace: "coder/dave/mail"}
			require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(), change, gateway.ViaUI))

			return func() {
				change := gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/mail", Servers: []string{"any-server", "optin-server"}}
				require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(), change, gateway.ViaUI))
			}
		}},
		{name: "secrets", path: "/ui/secrets", before: func() { seedScreenshotSecrets(t, env) }},
		{name: "secrets-delete-confirm", path: "/ui/secrets", prepare: chromedp.Click(chromedp.CSS(`#secrets tbody tr:first-child summary`))},
		{name: "secrets-empty", path: "/ui/secrets", before: func() {
			_, err := env.pool.Exec(t.Context(), `DELETE FROM user_secrets`)
			require.NoError(t, err)
		}},
		{name: "secrets-dialog", path: "/ui/secrets", prepare: chromedp.Click(chromedp.CSS(`main a[data-dialog="set-secret-dialog"]`))},
		{name: "secret-new", path: "/ui/secrets/new"},
		{name: "setup", path: "/ui/setup"},
		{name: "approvals", path: "/ui/approvals"},
		{name: "approval", path: "/ui/approvals/" + approvalID(1), cookie: "fresh-session"},
		{name: "approval-step-up", path: "/ui/approvals/" + approvalID(1), cookie: "stale-session"},
		{name: "grants", path: "/ui/grants", cookie: "dev-session"},
		{name: "grant-new", path: "/ui/grants/new", cookie: "dev-session"},
		{name: "audit", path: "/ui/audit"},
		{name: "audit-filters", path: "/ui/audit?outcome=refused&server=github&type=tool.decision"},
		{name: "audit-event-types", path: "/ui/audit", prepare: chromedp.Click(chromedp.CSS(`#filter details[data-popover] summary`))},
		{name: "audit-custom-range", path: "/ui/audit?range=custom"},
		{name: "audit-empty", path: "/ui/audit?server=github&tool=no-such-tool"},
		{name: "audit-more-filters", path: "/ui/audit?server=github", prepare: chromedp.Click(chromedp.CSS(`#more-filters summary`))},
		{name: "blocks", path: "/ui/blocks"},
		{name: "blocks-dialog", path: "/ui/blocks", prepare: chromedp.Click(chromedp.CSS(`main a[data-dialog="new-block"]`))},
		{name: "grants-dialog", path: "/ui/grants", cookie: "dev-session", prepare: chromedp.Click(chromedp.CSS(`main a[data-dialog="new-grant"]`))},
		{name: "grants-empty", path: "/ui/grants", cookie: "empty-session"},
		{name: "approvals-empty", path: "/ui/approvals", cookie: "empty-session"},
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
				cookie := page.cookie
				if cookie == "" {
					cookie = "admin-session"
				}

				secure := true
				_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
					Name: ui.SessionCookie, Value: cookie, URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
				})
				require.NoError(t, err)

				state := page.servers
				if state == "" {
					state = "seeded"
				}

				env.servers.setScreenshotState(state)

				if page.before != nil {
					page.before()
				}

				restore := func() {}
				if page.setup != nil {
					restore = page.setup()
				}

				steps := []chromedp.Action[chromedp.Void]{toolgatetest.Navigate(env.server.URL + page.path), chromedp.WaitReady(chromedp.CSS("main"))}
				if page.prepare != nil {
					steps = append(steps, page.prepare)
				}

				require.NoError(t, chromedp.Do(ctx, steps...), page.path)

				picture, err := chromedp.Run(ctx, chromedp.CaptureScreenshot())

				restore()
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
