//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// adminGroup is the group the e2e install makes web UI admins.
const adminGroup = "toolgate-admins"

const blocksPath = "/ui/blocks"

var (
	e2eAdmin = testutil.OIDCUser{Subject: "ui-admin", Email: "admin@example.com", EmailVerified: true, Groups: []string{adminGroup}}
	e2eUser  = testutil.OIDCUser{Subject: "ui-user", Email: "user@example.com", EmailVerified: true, Groups: []string{"developers"}}
)

// pageText returns the visible text of the page at path.
func pageText(browser context.Context, publicURL, path string) (string, error) {
	if err := browserDo(browser, testutil.Navigate(publicURL+path)); err != nil {
		return "", err
	}

	return browserRun(browser, testutil.BodyText())
}

func TestNonAdminHasNoBlocksPage(t *testing.T) {
	admin := adminClient(t)
	publicURL := webUI(t, forwardToGateway(t, admin, gatewayPods(t, admin)[0]))

	t.Run("a user signs in through the IdP, and without the admin group sees no Blocks link and gets 403 from /ui/blocks", func(t *testing.T) {
		browser := signInToUI(t, publicURL, e2eUser)

		hasLink, err := browserRun(browser, chromedp.Evaluate[bool](`document.querySelector('a[href="/ui/blocks"]') !== null`))
		require.NoError(t, err)
		assert.False(t, hasLink, "the navigation has no Blocks link")

		body, err := pageText(browser, publicURL, blocksPath)
		require.NoError(t, err)
		assert.Contains(t, body, "Only admins can open this page.")
	})

	t.Run("an admin sees the link and the page", func(t *testing.T) {
		browser := signInToUI(t, publicURL, e2eAdmin)

		hasLink, err := browserRun(browser, chromedp.Evaluate[bool](`document.querySelector('a[href="/ui/blocks"]') !== null`))
		require.NoError(t, err)
		assert.True(t, hasLink)

		body, err := pageText(browser, publicURL, blocksPath)
		require.NoError(t, err)
		assert.Contains(t, body, "Temporary blocks")
	})
}

func TestStandingRulesShown(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	publicURL := webUI(t, forwardToGateway(t, admin, gatewayPods(t, admin)[0]))

	rule := &v1alpha1.ToolgateRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: "e2e-rule-" + rand.String(5)},
		Spec: v1alpha1.ToolgateRuleSpec{
			Match:  v1alpha1.RuleMatch{Servers: []string{"no-such-server-" + rand.String(5)}},
			Reason: "Listed by the e2e test.",
		},
	}
	require.NoError(t, kube.Create(t.Context(), rule))
	t.Cleanup(func() {
		_ = kube.Delete(context.WithoutCancel(t.Context()), rule, client.PropagationPolicy(metav1.DeletePropagationBackground))
	})

	t.Run("the Blocks page lists the rule from the cluster, valid, with no form to change it", func(t *testing.T) {
		browser := signInToUI(t, publicURL, e2eAdmin)

		var body string

		require.Eventually(t, func() bool {
			var err error

			body, err = pageText(browser, publicURL, blocksPath)

			return err == nil && strings.Contains(body, rule.Namespace+"/"+rule.Name) && strings.Contains(body, "valid")
		}, time.Minute, time.Second, "the page lists the rule as valid")

		assert.Contains(t, body, "Listed by the e2e test.")

		forms, err := browserRun(browser, chromedp.Evaluate[int](`document.querySelectorAll('form:not([action="/ui/blocks"]):not([action="/ui/logout"])').length`))
		require.NoError(t, err)
		assert.Zero(t, forms, "the only forms add a block and sign out")
	})
}

// e2eBlockMinutes is the duration the e2e test enters in the block form. The
// e2e gateway stores it scaled, so the block lasts blockLifetime.
const e2eBlockMinutes = 3

// blockLifetime is how long the e2e test's block lasts.
func blockLifetime() time.Duration {
	return scaled(e2eBlockMinutes * time.Minute)
}

func TestTemporaryBlock(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := createTemplate(t, kube, "e2e-block", "per-user", testServerResources(t, image, 0, nil))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	publicURL := webUI(t, forwardToGateway(t, admin, pods[0]))

	t.Run("an admin's block applies to open sessions within 5 s and ends by itself at its expiry", func(t *testing.T) {
		session := connectGateway(t, forwardToGateway(t, admin, pods[len(pods)-1]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		browser := signInToUI(t, publicURL, e2eAdmin)
		// The form is in the page's "New block" dialog. The servers field
		// is a token input: Enter adds the typed name as an item.
		require.NoError(t, browserDo(browser,
			testutil.Navigate(publicURL+blocksPath),
			chromedp.Click(`main a[data-dialog="new-block"]`),
			chromedp.WaitVisible(`#new-block #field-servers`),
			chromedp.SendKeys(`#new-block #field-servers`, name+kb.Enter),
			chromedp.SendKeys(`#new-block #field-reason`, "Incident from the e2e test."),
			chromedp.SetValue(`#new-block #field-duration`, strconv.Itoa(e2eBlockMinutes)),
			chromedp.SetValue(`#new-block #field-unit`, "minutes"),
		))

		listChangedBefore := countEvents(session.recorded(), eventListChanged)
		submitted := time.Now()

		require.NoError(t, browserDo(browser, chromedp.Click(`#new-block button[type="submit"]`)))

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Greater(collect, countEvents(session.recorded(), eventListChanged), listChangedBefore)

			listed := listedTools(collect, t.Context(), session.session)
			for _, tool := range prefixedTools(name) {
				assert.NotContains(collect, listed, tool)
			}
		}, 5*time.Second, 100*time.Millisecond, "the open session lost the server's tools within 5 s of the form")

		body, err := pageText(browser, publicURL, blocksPath)
		require.NoError(t, err)
		assert.Contains(t, body, "Incident from the e2e test.")
		assert.Contains(t, body, "server "+name)

		// The block ends at its expiry: the tools come back with list_changed
		// and the session needs no new enable.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Subset(collect, listedTools(collect, t.Context(), session.session), prefixedTools(name))
		}, blockLifetime()+30*time.Second, time.Second, "the tools came back by themselves")
		assert.Less(t, time.Since(submitted), blockLifetime()+time.Minute)
	})
}
