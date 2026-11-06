//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// buildToolgate builds the CLI into a new directory and returns its path.
func buildToolgate(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "toolgate")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "github.com/solidDoWant/toolgate/cmd/toolgate")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))

	return binary
}

// workspaceEnvironment is the environment of a process in a workspace with no
// D-Bus session: nothing but the home directory and the toolgate settings.
func workspaceEnvironment(home string, extra ...string) []string {
	return append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, extra...)
}

func TestAC3_20DeviceSignInFromAWorkspace(t *testing.T) {
	provider := oidcProvider(t)
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	gatewayURL := strings.TrimSuffix(endpoint, gateway.MCPPath)
	binary := buildToolgate(t)

	subject := "workspace-" + rand.String(6)
	provider.AddUser(testutil.OIDCUser{Subject: subject, Email: subject + "@example.com", EmailVerified: true})

	// A server only this user may use.
	name := "e2e-device-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-session",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{subject}},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    testServerResources(t, image, 0, nil),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(t.Context(), template) })
	requireTemplateValid(t, kube, name)

	home := t.TempDir()

	// The login process: no D-Bus session, the file store.
	login := exec.CommandContext(t.Context(), binary, "login", "--gateway", gatewayURL, "--client-id", provider.ClientID, "--token-store", "file")
	login.Env = workspaceEnvironment(home)

	stdout, err := login.StdoutPipe()
	require.NoError(t, err)

	var stderr strings.Builder

	login.Stderr = &stderr
	require.NoError(t, login.Start())

	codePattern := regexp.MustCompile(`the code (\S+)$`)

	var output strings.Builder

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		output.WriteString(scanner.Text() + "\n")

		if match := codePattern.FindStringSubmatch(scanner.Text()); match != nil {
			require.NoError(t, provider.ApproveDevice(t.Context(), match[1], subject))
		}
	}

	require.NoError(t, login.Wait(), "stderr: %s", stderr.String())
	assert.Contains(t, output.String(), "The refresh token is stored in "+filepath.Join(home, ".config", "toolgate", "refresh-token"))

	runHeaders := func() (map[string]string, string, error) {
		command := exec.CommandContext(t.Context(), binary, "headers")
		// A later process, such as a Coder app, without the login's environment.
		command.Env = workspaceEnvironment(home, "TOOLGATE_SESSION=e2e-"+rand.String(8), "TOOLGATE_WORKSPACE=device-signin")

		var errOutput strings.Builder

		command.Stderr = &errOutput

		out, err := command.Output()
		values := map[string]string{}

		if err == nil {
			require.NoError(t, json.Unmarshal(out, &values))
		}

		return values, errOutput.String() + string(out), err
	}

	t.Run("[AC-3-20] a later toolgate headers gets a token and the session acts as the user", func(t *testing.T) {
		values, details, err := runHeaders()
		require.NoError(t, err, details)

		token := strings.TrimPrefix(values["Authorization"], "Bearer ")
		require.NotEmpty(t, token)

		session := connectWithToken(t, endpoint, token, values[auth.SessionHeader])

		// The template names only this user, so the catalog lists it for no one else.
		_, found := serverEntry(t, t.Context(), session.session, name)
		require.True(t, found, "the server lists only for the signed-in user")
	})

	t.Run("[AC-3-20] after the IdP revokes the refresh token, headers says to sign in again", func(t *testing.T) {
		provider.RevokeRefreshTokens(subject)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			values, details, err := runHeaders()
			assert.Error(collect, err)
			assert.Empty(collect, values)
			assert.Contains(collect, details, "Sign in again: run `toolgate login`.")
		}, 30*time.Second, time.Second)
	})
}
