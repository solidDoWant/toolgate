package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// runHook runs `toolgate hook session-start` with input on standard input.
func runHook(t *testing.T, input string) (int, string, string) {
	t.Helper()

	var stdout, stderr strings.Builder

	code := runWithInput([]string{"hook", "session-start"}, strings.NewReader(input), &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

// hookContext returns the additional context in the hook's output.
func hookContext(t *testing.T, stdout string) string {
	t.Helper()

	var output hookOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &output), "stdout: %s", stdout)
	assert.Equal(t, "SessionStart", output.HookSpecificOutput.HookEventName)

	return output.HookSpecificOutput.AdditionalContext
}

// startGatewayWithProfiles serves the gateway, with `/api/v1/settings`
// reading the profiles in kube, for the fake IdP.
func startGatewayWithProfiles(t *testing.T, provider *testutil.OIDCProvider, kube client.Reader) string {
	t.Helper()

	cfg := config.Default()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.MetricsListenAddress = "127.0.0.1:0"
	cfg.DatabaseURL = "postgres://toolgate@127.0.0.1:1/toolgate?connect_timeout=1"
	cfg.OIDCIssuerURL = provider.Issuer
	cfg.OIDCAudience = config.Audiences{provider.Audience}
	cfg.OIDCCLIClientID = provider.ClientID

	server, err := gateway.New(t.Context(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audiences: []string{provider.Audience}})
	require.NoError(t, err)

	handler := &settings.Handler{Reader: kube, Namespaces: []string{"toolgate"}, Log: slog.New(slog.DiscardHandler)}
	handler.Register(server, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

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

func profile(name string, groups []string, fragment string) *v1alpha1.SettingsProfile {
	return &v1alpha1.SettingsProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "toolgate", Name: name},
		Spec: v1alpha1.SettingsProfileSpec{
			Selector: v1alpha1.ProfileSelector{Groups: groups},
			Settings: runtime.RawExtension{Raw: []byte(fragment)},
		},
	}
}

func readSettingsFile(t *testing.T, path string) map[string]any {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	var values map[string]any
	require.NoError(t, json.Unmarshal(content, &values))

	return values
}

func writeSettingsFile(t *testing.T, path string, values map[string]any) {
	t.Helper()

	content, err := json.Marshal(values)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0o600))
}

func TestSettingsSync(t *testing.T) {
	t.Run("the SessionStart hook writes the profile's keys, leaves user edits, removes dropped keys and keeps other keys", func(t *testing.T) {
		isolateUserConfig(t)
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
		provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"dev"}})

		scheme := runtime.NewScheme()
		require.NoError(t, v1alpha1.AddToScheme(scheme))

		devProfile := profile("dev", []string{"dev"}, `{"model":"opus","env":{"FOO":"1","BAR":"2"},"permissions":{"deny":["Bash(rm:*)"]}}`)
		opsProfile := profile("ops", []string{"ops"}, `{"theme":"dark"}`)
		kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(devProfile, opsProfile).Build()

		gatewayURL := startGatewayWithProfiles(t, provider, kube)

		code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
		require.Equal(t, 0, code, "stderr: %s", stderr)

		home, err := os.UserHomeDir()
		require.NoError(t, err)

		settingsPath := filepath.Join(home, ".claude", "settings.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))
		writeSettingsFile(t, settingsPath, map[string]any{"editor": "vim", "env": map[string]any{"MINE": "x"}})

		code, stdout, stderr := runHook(t, `{"session_id":"session-1"}`)
		require.Equal(t, 0, code, "stderr: %s", stderr)
		assert.NotContains(t, hookContext(t, stdout), "changed by hand")

		assert.Equal(t, map[string]any{
			"editor":      "vim",
			"model":       "opus",
			"env":         map[string]any{"MINE": "x", "FOO": "1", "BAR": "2"},
			"permissions": map[string]any{"deny": []any{"Bash(rm:*)"}},
		}, readSettingsFile(t, settingsPath), "the matching profile's keys are written, and the ops profile's are not")

		// The user edits one key toolgate wrote, and the admin changes that
		// key, drops another and changes a third.
		values := readSettingsFile(t, settingsPath)
		values["model"] = "sonnet"
		writeSettingsFile(t, settingsPath, values)

		devProfile.Spec.Settings.Raw = []byte(`{"model":"haiku","env":{"FOO":"3"},"permissions":{"deny":["Bash(rm:*)"]}}`)
		require.NoError(t, kube.Update(t.Context(), devProfile))

		code, stdout, stderr = runHook(t, `{"session_id":"session-2"}`)
		require.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Contains(t, hookContext(t, stdout), "model", "the user-changed key is reported")

		assert.Equal(t, map[string]any{
			"editor":      "vim",
			"model":       "sonnet",
			"env":         map[string]any{"MINE": "x", "FOO": "3"},
			"permissions": map[string]any{"deny": []any{"Bash(rm:*)"}},
		}, readSettingsFile(t, settingsPath))
	})
}

func TestHookSignedOutTellsTheUserToSignIn(t *testing.T) {
	isolateUserConfig(t)

	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("CLAUDE_PID", "4242")

	code, stdout, stderr := runHook(t, `{"session_id":"session-1","hook_event_name":"SessionStart"}`)
	assert.Equal(t, 0, code, "the hook never blocks the session")
	assert.Empty(t, stderr)
	assert.Contains(t, hookContext(t, stdout), "Run `toolgate login`")

	content, err := os.ReadFile(filepath.Join(runtimeDir, "toolgate", strconv.Itoa(4242)))
	require.NoError(t, err, "the session file is written even when signed out")
	assert.Equal(t, "session-1\n", string(content))
}

func TestFetchSettingsTreatsAGatewayWithoutTheRouteAsNoProfile(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	profile, err := fetchSettings(t.Context(), server.URL, "token", "")
	require.NoError(t, err)
	assert.Empty(t, profile)
}
