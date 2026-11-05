package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

var userCodePattern = regexp.MustCompile(`code (\S+)`)

// isolateUserConfig points os.UserConfigDir at a new directory and the
// keychain at an in-memory mock, and returns toolgate's directory in it.
func isolateUserConfig(t *testing.T) string {
	t.Helper()

	keyring.MockInit()

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)

	dir, err := configDir()
	require.NoError(t, err)

	return dir
}

// startGateway serves the gateway's main handler for the fake IdP.
func startGateway(t *testing.T, provider *testutil.OIDCProvider, clientID string) string {
	t.Helper()

	cfg := config.Default()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.MetricsListenAddress = "127.0.0.1:0"
	cfg.DatabaseURL = "postgres://toolgate@127.0.0.1:1/toolgate?connect_timeout=1"
	cfg.OIDCIssuerURL = provider.Issuer
	cfg.OIDCAudience = provider.Audience
	cfg.OIDCCLIClientID = clientID

	server, err := gateway.New(t.Context(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, server.Listen())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = server.Serve(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return "http://" + server.Addr().String()
}

// signInApproving runs `toolgate login` with args and approves the user code
// it prints as subject. It returns the exit code and the output.
func signInApproving(t *testing.T, provider *testutil.OIDCProvider, subject string, args ...string) (int, string, string) {
	t.Helper()

	reader, writer := io.Pipe()

	var stderr strings.Builder

	result := make(chan int, 1)

	go func() {
		code := run(append([]string{"login"}, args...), writer, &stderr)
		_ = writer.Close()

		result <- code
	}()

	var output strings.Builder

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		output.WriteString(line + "\n")

		if match := userCodePattern.FindStringSubmatch(line); match != nil {
			assert.NoError(t, provider.ApproveDevice(t.Context(), match[1], subject))
		}
	}

	select {
	case code := <-result:
		return code, output.String(), stderr.String()
	case <-time.After(time.Minute):
		t.Fatal("toolgate login did not finish")

		return 0, "", ""
	}
}

func TestAC1_02LoginThenStatusShowsEmail(t *testing.T) {
	t.Run("[AC-1-02] login with the device flow, then status shows the email", func(t *testing.T) {
		dir := isolateUserConfig(t)

		provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
		provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
		gatewayURL := startGateway(t, provider, provider.ClientID)

		code, output, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
		require.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Contains(t, output, "Signed in as alice@example.com.")

		refreshToken, err := tokenstore.New(dir).Load()
		require.NoError(t, err)
		assert.NotEmpty(t, refreshToken)

		settingsFile, err := os.ReadFile(filepath.Join(dir, "config.json"))
		require.NoError(t, err)
		assert.NotContains(t, string(settingsFile), refreshToken, "config.json holds no token")

		var stdout, statusErr strings.Builder

		require.Equal(t, 0, run([]string{"status"}, &stdout, &statusErr), "stderr: %s", statusErr.String())
		assert.Contains(t, stdout.String(), "Gateway:   "+gatewayURL)
		assert.Contains(t, stdout.String(), "Principal: alice")
		assert.Contains(t, stdout.String(), "Email:     alice@example.com\n")
	})
}

func TestStatusMarksUnverifiedEmail(t *testing.T) {
	isolateUserConfig(t)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "bob", Email: "bob@example.com"})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "bob", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	var stdout, statusErr strings.Builder

	require.Equal(t, 0, run([]string{"status"}, &stdout, &statusErr), "stderr: %s", statusErr.String())
	assert.Contains(t, stdout.String(), "Email:     bob@example.com (unverified)\n")
}

func TestLoginClientIDFlagOverridesGateway(t *testing.T) {
	isolateUserConfig(t)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "carol", Email: "carol@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, "")

	code, _, stderr := signInApproving(t, provider, "carol", "--gateway", gatewayURL)
	require.Equal(t, 1, code)
	assert.Contains(t, stderr, "pass --client-id")

	code, _, stderr = signInApproving(t, provider, "carol", "--gateway", gatewayURL, "--client-id", provider.ClientID)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	// A second login without --gateway reuses the saved gateway.
	code, _, stderr = signInApproving(t, provider, "carol", "--client-id", provider.ClientID)
	require.Equal(t, 0, code, "stderr: %s", stderr)
}

func TestStatusWithoutSignIn(t *testing.T) {
	isolateUserConfig(t)

	var stdout, stderr strings.Builder

	assert.Equal(t, 1, run([]string{"status"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Not signed in. Run toolgate login.")
}
