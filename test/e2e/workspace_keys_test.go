//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

var approveItPattern = regexp.MustCompile(`Approve it at (\S+?)\. Make sure`)

// scanForMatch starts command, calls onMatch for each output line that
// matches pattern, and returns the whole output once the output closes.
func scanForMatch(t *testing.T, command *exec.Cmd, pattern *regexp.Regexp, onMatch func(match []string)) string {
	t.Helper()

	stdout, err := command.StdoutPipe()
	require.NoError(t, err)

	var output strings.Builder

	require.NoError(t, command.Start())

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		output.WriteString(scanner.Text() + "\n")

		if match := pattern.FindStringSubmatch(scanner.Text()); match != nil {
			onMatch(match)
		}
	}

	return output.String()
}

func TestWorkspaceKeyAttestation(t *testing.T) {
	provider := oidcProvider(t)
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	webUI(t, endpoint)
	apiURL := strings.TrimSuffix(endpoint, gateway.MCPPath)
	binary := buildToolgate(t)

	subject := "wk-" + rand.String(6)
	user := testutil.OIDCUser{Subject: subject, Email: subject + "@example.com", EmailVerified: true, PreferredUsername: subject}
	provider.AddUser(user)

	const keyName = "dev"

	attestedServer := attestationTemplate(t, kube, "e2e-wk-attested", "per-session",
		v1alpha1.Access{Users: []string{subject}, Environments: "attested"}, testServerResources(t, image, 0, nil))
	optInServer := attestationTemplate(t, kube, "e2e-wk-optin", "per-session",
		v1alpha1.Access{Users: []string{subject}, Environments: "opt-in"}, testServerResources(t, image, 0, nil))

	home := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "keys", keyName+".pem")

	// Sign in on the command line.
	login := exec.CommandContext(t.Context(), binary, "login", "--gateway", apiURL, "--client-id", provider.ClientID, "--token-store", "file")
	login.Env = workspaceEnvironment(home)

	scanForMatch(t, login, regexp.MustCompile(`the code (\S+)$`), func(match []string) {
		require.NoError(t, provider.ApproveDevice(t.Context(), match[1], subject))
	})
	require.NoError(t, login.Wait())

	// Register the key and approve it in the browser with a fresh sign-in.
	register := exec.CommandContext(t.Context(), binary, "workspace", "register", keyName, "--key-file", keyFile)
	register.Env = workspaceEnvironment(home)

	browser := uiBrowser(t)
	approved := false
	registerOutput := scanForMatch(t, register, approveItPattern, func(match []string) {
		link := match[1]
		_, page := openApprovalLink(t, browser, link, user)
		require.Contains(t, page, "SHA256:", "the page shows the key's fingerprint")

		response, err := browser.Get(link)
		require.NoError(t, err)

		page = readPage(t, response)
		token := csrfTokenPattern.FindStringSubmatch(page)
		require.NotNil(t, token, page)

		response, err = browser.PostForm(link+"/approve", url.Values{"csrf_token": {token[1]}})
		require.NoError(t, err)

		page = readPage(t, response)
		require.Equal(t, http.StatusOK, response.StatusCode, page)
		assert.Contains(t, page, "approved")

		approved = true
	})
	require.NoError(t, register.Wait(), registerOutput)
	require.True(t, approved, registerOutput)
	assert.Contains(t, registerOutput, "Sessions that use it run in workspace `registered/"+subject+"/"+keyName+"`")

	configure := exec.CommandContext(t.Context(), binary, "configure", "--attestation-key-file", keyFile)
	configure.Env = workspaceEnvironment(home)

	out, err := configure.CombinedOutput()
	require.NoError(t, err, string(out))

	runHeaders := func(t *testing.T) http.Header {
		t.Helper()

		command := exec.CommandContext(t.Context(), binary, "headers")
		command.Env = workspaceEnvironment(home, "TOOLGATE_SESSION=e2e-"+rand.String(8))

		output, err := command.Output()
		require.NoError(t, err)

		values := map[string]string{}
		require.NoError(t, json.Unmarshal(output, &values))

		header := http.Header{}
		for name, value := range values {
			header.Set(name, value)
		}

		return header
	}

	identityOf := func(t *testing.T, header http.Header) string {
		t.Helper()

		var identity string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			identity = gatewayQuery(t, admin, "SELECT DISTINCT attestation || ' ' || workspace FROM audit_events "+
				"WHERE session = '"+header.Get(auth.SessionHeader)+"' AND type = 'session.start'")
			assert.NotEmpty(collect, identity)
		}, time.Minute, time.Second)

		return strings.TrimSpace(identity)
	}

	header := runHeaders(t)
	require.NotEmpty(t, header.Get(auth.AttestationHeader), "headers sends the key assertion")

	t.Run("a session through toolgate headers is attested in the registered workspace", func(t *testing.T) {
		session := connectWithHeader(t, endpoint, header)

		assert.Equal(t, "attested registered/"+subject+"/"+keyName, identityOf(t, header))

		for _, name := range []string{attestedServer, optInServer} {
			entry, found := serverEntry(t, t.Context(), session.session, name)
			require.True(t, found, name)
			assert.NotEqual(t, gateway.StatusBlocked, entry.Status, "%s needs no unverified grant or allow list: %s", name, entry.Reason)
		}
	})

	t.Run("another user's token with this key gives an unverified session", func(t *testing.T) {
		other := "wk-other-" + rand.String(5)
		stolen := attestedHeader(t, other, nil, header.Get(auth.AttestationHeader), "")
		connectWithHeader(t, endpoint, stolen)

		assert.Equal(t, "user claimed/default", identityOf(t, stolen))

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			count := gatewayQuery(t, admin, "SELECT count(*) FROM audit_events WHERE type = 'auth.refuse' AND principal = '"+other+"' "+
				"AND details->>'reason' = 'attestation key owner mismatch'")
			assert.NotEqual(collect, "0", strings.TrimSpace(count))
		}, time.Minute, time.Second)
	})
}
