package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// bearerServer is an MCP server that answers only requests carrying one
// bearer token, and remembers every Authorization header it got.
type bearerServer struct {
	URL string

	mu   sync.Mutex
	seen []string
}

// newBearerServer starts an MCP server with one tool, `search`, that
// refuses any request without `Authorization: Bearer <token>` with status.
func newBearerServer(t *testing.T, token string, stateless bool, status int) *bearerServer {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: stateless})

	bearer := &bearerServer{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := request.Header.Get("Authorization")

		bearer.mu.Lock()
		bearer.seen = append(bearer.seen, got)
		bearer.mu.Unlock()

		if got != "Bearer "+token {
			http.Error(writer, "no", status)

			return
		}

		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	bearer.URL = httpServer.URL + "/mcp"

	return bearer
}

func (b *bearerServer) authorizations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.seen...)
}

func remoteTemplate(url string) *v1alpha1.MCPServerTemplate {
	template := testTemplate()
	template.Spec.Endpoint = v1alpha1.Endpoint{}
	template.Spec.Remote = &v1alpha1.RemoteServer{
		URL:     url,
		Headers: map[string]string{"Authorization": "Bearer ${secret:archive-key}", "X-User": "${principal.id}"},
		Timeout: &metav1.Duration{Duration: 5 * time.Second},
	}

	return template
}

// userSecrets is a SecretGetter over principal → name → value.
func userSecrets(values map[string]map[string]string) runtime.SecretGetter {
	return func(_ context.Context, principal, name string) ([]byte, time.Time, error) {
		value, found := values[principal][name]
		if !found {
			return nil, time.Time{}, secrets.ErrNotFound
		}

		return []byte(value), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), nil
	}
}

func TestRemoteHeadersFillSecretsAndPrincipal(t *testing.T) {
	template := remoteTemplate("https://archive.example.com/mcp")
	source := runtime.BrokerSecrets(userSecrets(map[string]map[string]string{"alice": {"archive-key": "ma_alice"}}))

	header, versions, err := runtime.RemoteHeaders(t.Context(), template, runtime.Identity{PrincipalID: "alice"}, source("alice"))
	require.NoError(t, err)
	assert.Equal(t, "Bearer ma_alice", header.Get("Authorization"))
	assert.Equal(t, "alice", header.Get("X-User"))
	assert.Contains(t, versions, "archive-key")
}

func TestRemoteHeadersMissingSecretTellsHowToSetIt(t *testing.T) {
	template := remoteTemplate("https://archive.example.com/mcp")
	source := runtime.BrokerSecrets(userSecrets(nil))

	_, _, err := runtime.RemoteHeaders(t.Context(), template, runtime.Identity{PrincipalID: "alice"}, source("alice"))

	var missing *runtime.MissingSecretError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, "Secret `archive-key` isn't set. Run `toolgate secret set archive-key`.", err.Error())
}

func TestRemoteHeadersRefuseAValueThatBreaksTheHeader(t *testing.T) {
	template := remoteTemplate("https://archive.example.com/mcp")
	source := runtime.BrokerSecrets(userSecrets(map[string]map[string]string{"alice": {"archive-key": "ma_alice\r\nX-Injected: yes"}}))

	_, _, err := runtime.RemoteHeaders(t.Context(), template, runtime.Identity{PrincipalID: "alice"}, source("alice"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ma_alice")
}

// remoteStarterFixture is a Starter fixture for a remote template whose
// tool lister speaks MCP to url, with alice's key set to key.
func remoteStarterFixture(t *testing.T, url, key string, logs *bytes.Buffer) *starterFixture {
	t.Helper()

	fixture := newStarterFixture(t, remoteTemplate(url), "pending")
	fixture.instance.Namespace = ""

	values := map[string]map[string]string{}
	if key != "" {
		values["alice"] = map[string]string{"archive-key": key}
	}

	fixture.starter.Secrets = runtime.BrokerSecrets(userSecrets(values))
	fixture.starter.ListTools = runtime.MCPToolLister
	fixture.starter.Client = nil
	fixture.starter.Logger = debugLogger(logs)

	return fixture
}

// stepUntilSettled runs the Starter until the instance is ready or failed.
func stepUntilSettled(t *testing.T, fixture *starterFixture) {
	t.Helper()

	for range 3 {
		status, _, _, _ := fixture.store.snapshot()
		if status == "ready" || status == "failed" {
			return
		}

		fixture.instance.Status = status
		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))
	}
}

func TestRemoteStartNeverRevealsHeaderValues(t *testing.T) {
	const secret = "ma_secret_value_that_must_not_leak"

	tests := []struct {
		name       string
		key        string
		wantStatus string
	}{
		{name: "accepted key", key: secret, wantStatus: "ready"},
		{name: "refused key", key: secret + "-wrong", wantStatus: "failed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newBearerServer(t, secret, false, http.StatusUnauthorized)

			var logs bytes.Buffer

			fixture := remoteStarterFixture(t, upstream.URL, test.key, &logs)
			stepUntilSettled(t, fixture)

			status, reason, tools, _ := fixture.store.snapshot()
			require.Equal(t, test.wantStatus, status, reason)

			for _, text := range []string{logs.String(), reason, string(tools), fixture.store.unhealthyReason} {
				assert.NotContains(t, text, test.key)
			}
		})
	}
}

func TestRemoteStartSendsTheOwnersHeaders(t *testing.T) {
	upstream := newBearerServer(t, "ma_alice", false, http.StatusUnauthorized)

	fixture := remoteStarterFixture(t, upstream.URL, "ma_alice", &bytes.Buffer{})
	stepUntilSettled(t, fixture)

	status, reason, tools, _ := fixture.store.snapshot()
	require.Equal(t, "ready", status, reason)
	assert.Contains(t, string(tools), `"search"`)

	authorizations := upstream.authorizations()
	require.NotEmpty(t, authorizations)

	for _, authorization := range authorizations {
		assert.Equal(t, "Bearer ma_alice", authorization)
	}
}

func TestRemoteStartCreatesNoKubernetesObjects(t *testing.T) {
	upstream := newBearerServer(t, "ma_alice", false, http.StatusUnauthorized)

	fixture := remoteStarterFixture(t, upstream.URL, "ma_alice", &bytes.Buffer{})
	// A nil client panics on any Kubernetes call.
	stepUntilSettled(t, fixture)

	status, reason, _, _ := fixture.store.snapshot()
	require.Equal(t, "ready", status, reason)
	assert.Empty(t, fixture.store.healthCheck)
}

func TestMonitorPingsRemoteServersWithTheOwnersHeaders(t *testing.T) {
	upstream := newBearerServer(t, "ma_alice", false, http.StatusUnauthorized)
	template := remoteTemplate(upstream.URL)
	instance := db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", OwnerPrincipal: "alice", Tools: []byte(`[]`),
	}
	store := &healthStore{row: instance}

	var filledFor []string

	var filledMu sync.Mutex

	monitor := &runtime.Monitor{
		Store: store,
		Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			return template.DeepCopy(), true
		},
		Connect: runtime.MCPMonitorConnector,
		RemoteAuth: func(_ context.Context, instance db.Instance, _ *v1alpha1.MCPServerTemplate) (*runtime.RemoteAuth, error) {
			filledMu.Lock()

			filledFor = append(filledFor, instance.OwnerPrincipal)
			filledMu.Unlock()

			return &runtime.RemoteAuth{Header: http.Header{"Authorization": {"Bearer ma_alice"}}}, nil
		},
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		require.NoError(t, monitor.Step(t.Context(), instance))

		return len(upstream.authorizations()) >= 3
	}, 10*time.Second, 10*time.Millisecond)

	for _, authorization := range upstream.authorizations() {
		assert.Equal(t, "Bearer ma_alice", authorization)
	}

	row, _ := store.snapshot()
	assert.Equal(t, "ready", row.Status, row.Reason)

	filledMu.Lock()
	defer filledMu.Unlock()

	assert.Contains(t, filledFor, "alice")
}

func TestMonitorFailsARemoteInstanceWhoseSecretIsGone(t *testing.T) {
	template := remoteTemplate("https://archive.example.com/mcp")
	instance := db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", OwnerPrincipal: "alice",
	}
	store := &healthStore{row: instance}
	monitor := &runtime.Monitor{
		Store: store,
		Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			return template.DeepCopy(), true
		},
		Connect: func(context.Context, string, string, func()) (runtime.MonitorSession, error) {
			return nil, errors.New("the monitor must not connect without headers")
		},
		RemoteAuth: func(context.Context, db.Instance, *v1alpha1.MCPServerTemplate) (*runtime.RemoteAuth, error) {
			return nil, &runtime.MissingSecretError{Name: "archive-key"}
		},
		Interval: time.Hour,
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		require.NoError(t, monitor.Step(t.Context(), instance))

		row, _ := store.snapshot()

		return row.Status == "failed"
	}, 10*time.Second, 10*time.Millisecond)

	row, _ := store.snapshot()
	assert.Equal(t, "Secret `archive-key` isn't set. Run `toolgate secret set archive-key`.", row.Reason)
}

// debugLogger logs everything, debug included, to logs as JSON.
func debugLogger(logs *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
