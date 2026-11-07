//go:build e2e

package e2e

import (
	"context"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// webUI serves a TLS proxy to a gateway pod at the web UI's public URL
// (TOOLGATE_E2E_UI_LISTEN) and returns that URL. It skips the test when the
// harness configured no web UI.
func webUI(t *testing.T, gatewayEndpoint string) string {
	t.Helper()

	address := os.Getenv("TOOLGATE_E2E_UI_LISTEN")
	if address == "" {
		t.Skip("TOOLGATE_E2E_UI_LISTEN is not set: no web UI (run through scripts/e2e/run.sh)")
	}

	target, err := url.Parse(strings.TrimSuffix(gatewayEndpoint, gateway.MCPPath))
	require.NoError(t, err)

	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)

	// The proxy keeps the browser's Host header, so the gateway's
	// cross-origin check sees the public URL's host.
	server := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(server.Close)

	return "https://" + address
}

// signInToUI opens the web UI's Servers page in a new browser and signs in at
// the fake identity provider as user.
func signInToUI(t *testing.T, publicURL string, user testutil.OIDCUser) context.Context {
	t.Helper()

	oidcProvider(t)

	browser := testutil.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	require.NoError(t, chromedp.Do(browser, testutil.Navigate(publicURL+ui.HomePath), testutil.SignInToOIDC(user)))

	require.Eventually(t, func() bool {
		title, err := chromedp.Run(browser, chromedp.Title())

		return err == nil && title == "Servers · toolgate"
	}, time.Minute, 200*time.Millisecond, "the browser is back on the Servers page, signed in")

	return browser
}

func TestRestartFromTheUI(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := createTemplate(t, kube, "e2e-restart", "per-user", testServerResources(t, image, 0, map[string]string{"VERSION": "v1"}))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	publicURL := webUI(t, forwardToGateway(t, admin, pods[0]))

	t.Run("a user restarts their outdated instance; the new one runs the current version and the session keeps the server", func(t *testing.T) {
		session := connectGateway(t, forwardToGateway(t, admin, pods[1]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		old := requireOneInstance(t, admin, name)
		oldID := old.Labels[runtime.LabelInstance]

		var environment map[string]string

		structuredResult(t, session.session, name+"__env", &environment)
		require.Equal(t, "v1", environment["VERSION"])

		// A new template version makes the running instance outdated.
		var template v1alpha1.MCPServerTemplate
		require.NoError(t, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))
		template.Spec.Resources = testServerResources(t, image, 0, map[string]string{"VERSION": "v2"})
		require.NoError(t, kube.Update(t.Context(), &template))

		browser := signInToUI(t, publicURL, testutil.OIDCUser{
			Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"developers"},
		})

		restart := chromedp.CSS(`form[action="/ui/instances/` + oldID + `/restart"] button`)

		require.Eventually(t, func() bool {
			if err := chromedp.Do(browser, testutil.Navigate(publicURL+ui.HomePath)); err != nil {
				return false
			}

			found, err := chromedp.Run(browser, chromedp.Evaluate[bool](
				`document.querySelector('form[action="/ui/instances/`+oldID+`/restart"] button') !== null`))

			return err == nil && found
		}, 2*time.Minute, time.Second, "the Servers page offers a restart once the instance is outdated")

		listChangedBefore := countEvents(session.recorded(), eventListChanged)

		require.NoError(t, chromedp.Do(browser, chromedp.Click(restart)))
		require.Eventually(t, func() bool {
			body, err := chromedp.Run(browser, testutil.BodyText())

			return err == nil && strings.Contains(body, "The restart was requested.")
		}, time.Minute, 200*time.Millisecond, "the browser shows the request was taken")

		// The replacement runs in a new namespace; the old one goes.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			require.Len(collect, namespaces, 1)
			assert.NotEqual(collect, oldID, namespaces[0].Labels[runtime.LabelInstance])
		}, enableTimeout, time.Second, "one instance namespace, the replacement's")

		// The session keeps the server enabled: its tools come back with
		// list_changed once the new instance is ready.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Greater(collect, countEvents(session.recorded(), eventListChanged), listChangedBefore)
			assert.Subset(collect, listedTools(collect, t.Context(), session.session), prefixedTools(name))
		}, enableTimeout, time.Second, "the session lists the server's tools again")

		structuredResult(t, session.session, name+"__env", &environment)
		assert.Equal(t, "v2", environment["VERSION"], "the new instance runs the current template version")
	})
}

func countEvents(events []string, event string) int {
	return len(slices.DeleteFunc(slices.Clone(events), func(recorded string) bool { return recorded != event }))
}
