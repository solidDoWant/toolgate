//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// uiPort is the local port the chart's web UI public URL points at.
func uiPort(t *testing.T) int {
	t.Helper()

	port, err := strconv.Atoi(os.Getenv("TOOLGATE_E2E_UI_PORT"))
	if err != nil {
		t.Skip("TOOLGATE_E2E_UI_PORT is not set (run through scripts/e2e/run.sh)")
	}

	return port
}

// secureJar keeps and sends `Secure` cookies over plain HTTP, as browsers do
// for 127.0.0.1, so the web UI's `__Host-` cookies work in the e2e install.
type secureJar struct{ jar *cookiejar.Jar }

func asHTTPS(target *url.URL) *url.URL {
	secure := *target
	secure.Scheme = "https"

	return &secure
}

func (s secureJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	s.jar.SetCookies(asHTTPS(target), cookies)
}

func (s secureJar) Cookies(target *url.URL) []*http.Cookie {
	return s.jar.Cookies(asHTTPS(target))
}

// uiBrowser is an HTTP client with cookies that follows redirects.
func uiBrowser(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{Jar: secureJar{jar: jar}, Timeout: 30 * time.Second}
}

func readPage(t *testing.T, response *http.Response) string {
	t.Helper()

	defer func() { _ = response.Body.Close() }()

	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return string(content)
}

// openApprovalLink opens an approval link without a UI session, signs in at
// the fake IdP as user where the gateway sends the browser, and returns the
// page the sign-in ends on.
func openApprovalLink(t *testing.T, browser *http.Client, link string, user testutil.OIDCUser) (*http.Response, string) {
	t.Helper()

	response, err := browser.Get(link)
	require.NoError(t, err)
	_ = readPage(t, response)
	require.Equal(t, testutil.OIDCSignInPath, response.Request.URL.Path, "the link goes straight to the IdP's sign-in")
	assert.True(t, redirectedThrough(response, func(hop *url.URL) bool { return hop.Query().Get("prompt") == "login" }),
		"the sign-in is a step-up")

	signIn := response.Request.URL
	response, err = browser.PostForm(signIn.String(), url.Values{
		"authRequestID":  {signIn.Query().Get("authRequestID")},
		"subject":        {user.Subject},
		"email":          {user.Email},
		"email_verified": {"true"},
	})
	require.NoError(t, err)

	return response, readPage(t, response)
}

// redirectedThrough reports whether a request on response's redirect chain
// went to a URL that matches.
func redirectedThrough(response *http.Response, matches func(*url.URL) bool) bool {
	for request := response.Request; request != nil; {
		if matches(request.URL) {
			return true
		}

		if request.Response == nil {
			return false
		}

		request = request.Response.Request
	}

	return false
}

var (
	approvalLinkPattern = regexp.MustCompile("Approve request `([0-9a-f]{32})` at (\\S+),")
	csrfTokenPattern    = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
)

func callText(result *mcp.CallToolResult) string {
	var text strings.Builder

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text.WriteString(textContent.Text)
		}
	}

	return text.String()
}

func TestAC2_12ApprovalOnThePage(t *testing.T) {
	image := mcpServerImage(t)
	port := uiPort(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := "e2e-approval-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice"}, Environments: "any"},
			Tools:        map[string]v1alpha1.ToolOverride{"echo": {Approval: "required"}},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    testServerResources(t, image, 0, nil),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	endpoint := forwardToGatewayOn(t, admin, gatewayPods(t, admin)[0], port)
	gatewayURL := strings.TrimSuffix(endpoint, gateway.MCPPath)

	alice := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
	enable(t, alice, name)

	call := &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "approve me"}}

	var id, link string

	t.Run("[AC-2-12] without elicitation the call fails at once naming the approval", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		result, err := alice.session.CallTool(ctx, call)
		require.NoError(t, err)
		require.True(t, result.IsError)

		match := approvalLinkPattern.FindStringSubmatch(callText(result))
		require.NotNil(t, match, "the error names the approval and its link: %s", callText(result))

		id, link = match[1], match[2]
		assert.Equal(t, gatewayURL+"/ui/approvals/"+id, link)
	})

	require.NotEmpty(t, id)

	browser := uiBrowser(t)

	t.Run("[AC-2-12] the page lists the approval", func(t *testing.T) {
		response, page := openApprovalLink(t, browser, link, testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
		require.Equal(t, http.StatusOK, response.StatusCode, page)
		assert.Equal(t, "/ui/approvals/"+id, response.Request.URL.Path, "the sign-in returns to the approval")

		response, err := browser.Get(gatewayURL + "/ui/approvals")
		require.NoError(t, err)

		list := readPage(t, response)
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.Contains(t, list, "/ui/approvals/"+id)
		assert.Contains(t, list, "<td>"+name+"</td><td>echo</td>")
	})

	t.Run("[AC-2-12] after approving, the retry succeeds", func(t *testing.T) {
		response, err := browser.Get(link)
		require.NoError(t, err)

		page := readPage(t, response)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Contains(t, page, "/ui/approvals/"+id+"/approve", "a fresh sign-in shows the approve form")

		token := csrfTokenPattern.FindStringSubmatch(page)
		require.NotNil(t, token)

		response, err = browser.PostForm(link+"/approve", url.Values{"csrf_token": {token[1]}})
		require.NoError(t, err)

		page = readPage(t, response)
		require.Equal(t, http.StatusOK, response.StatusCode, page)
		assert.Contains(t, page, "This approval was approved.")

		result, err := alice.session.CallTool(t.Context(), call)
		require.NoError(t, err)
		require.False(t, result.IsError, callText(result))
		assert.Contains(t, callText(result), "approve me")
	})
}
