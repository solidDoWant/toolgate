//go:build integration

package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// waitTimeout bounds every wait for a condition in these tests.
const waitTimeout = 30 * time.Second

// enableTimeout bounds the wait for an enable to return. It is shorter than
// the 10 s progress tick, so an enable that wakes only on the tick, not on
// the notification, fails the test.
const enableTimeout = 8 * time.Second

var anyone = v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"}

// observedClient is an MCP client session that records the notifications
// it receives.
type observedClient struct {
	session *mcp.ClientSession

	mu          sync.Mutex
	listChanged int
	progress    []string
}

func (o *observedClient) listChangedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.listChanged
}

func (o *observedClient) progressMessages() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string(nil), o.progress...)
}

func (f *mcpFixture) connectObserved(t *testing.T, client user) *observedClient {
	t.Helper()

	return f.connectObservedTo(t, f.server.URL, client)
}

// connectObservedTo connects client to the gateway replica at endpoint.
func (f *mcpFixture) connectObservedTo(t *testing.T, endpoint string, client user) *observedClient {
	t.Helper()

	observed := &observedClient{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			observed.mu.Lock()
			observed.listChanged++
			observed.mu.Unlock()
		},
		ProgressNotificationHandler: func(_ context.Context, request *mcp.ProgressNotificationClientRequest) {
			observed.mu.Lock()
			observed.progress = append(observed.progress, request.Params.Message)
			observed.mu.Unlock()
		},
	})
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerTransport{header: f.headers(t, client)}},
	}

	session, err := mcpClient.Connect(t.Context(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	observed.session = session

	return observed
}

// enableResult is the outcome of one toolgate_enable_server call.
type enableResult struct {
	tools []string
	err   string
}

// startEnable calls toolgate_enable_server in the background, with a
// progress token.
func startEnable(t *testing.T, session *mcp.ClientSession, name string) <-chan enableResult {
	t.Helper()

	results := make(chan enableResult, 1)
	ctx := t.Context()

	go func() {
		params := &mcp.CallToolParams{Name: gateway.ToolEnableServer, Arguments: map[string]any{"name": name}}
		params.SetProgressToken("enable-" + name)

		result, err := session.CallTool(ctx, params)
		results <- decodeEnable(result, err)
	}()

	return results
}

func decodeEnable(result *mcp.CallToolResult, err error) enableResult {
	if err != nil {
		return enableResult{err: err.Error()}
	}

	if result.IsError {
		text := ""

		for _, content := range result.Content {
			if textContent, ok := content.(*mcp.TextContent); ok {
				text += textContent.Text
			}
		}

		return enableResult{err: text}
	}

	encoded, _ := json.Marshal(result.StructuredContent)

	var decoded gateway.EnableResult

	_ = json.Unmarshal(encoded, &decoded)

	return enableResult{tools: decoded.Tools}
}

func awaitEnable(t *testing.T, results <-chan enableResult) enableResult {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(enableTimeout):
		require.FailNow(t, "enable did not return")

		return enableResult{}
	}
}

func disable(t *testing.T, session *mcp.ClientSession, name string) {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolDisableServer, Arguments: map[string]any{"name": name}})
	require.NoError(t, err)
	require.False(t, result.IsError, "disable failed: %v", result.Content)
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}

	return names
}

var metaToolNames = []string{gateway.ToolListServers, gateway.ToolEnableServer, gateway.ToolDisableServer, gateway.ToolServerStatus}

// instancesOf returns every instance row of template.
func (f *mcpFixture) instancesOf(t *testing.T, template *v1alpha1.MCPServerTemplate) []db.Instance {
	t.Helper()

	all, err := db.New(f.pool).ListInstancesNotStopped(t.Context())
	require.NoError(t, err)

	var rows []db.Instance

	for _, instance := range all {
		if instance.TemplateNs == template.Namespace && instance.TemplateName == template.Name {
			rows = append(rows, instance)
		}
	}

	return rows
}

// awaitInstance waits until template has an instance row for scopeKey.
func (f *mcpFixture) awaitInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, scopeKey string) db.Instance {
	t.Helper()

	var found db.Instance

	require.Eventually(t, func() bool {
		for _, instance := range f.instancesOf(t, template) {
			if instance.ScopeKey == scopeKey {
				found = instance

				return true
			}
		}

		return false
	}, waitTimeout, 10*time.Millisecond)

	return found
}

// markReady does what the reconciler does when an instance becomes ready:
// it stores the tool list and notifies, in one transaction.
func (f *mcpFixture) markReady(t *testing.T, id string, tools ...string) {
	t.Helper()

	listed := make([]*mcp.Tool, 0, len(tools))
	for _, name := range tools {
		listed = append(listed, &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}})
	}

	encoded, err := json.Marshal(listed)
	require.NoError(t, err)

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		instance, err := queries.GetInstance(t.Context(), id)
		if err != nil {
			return err
		}

		if _, err := queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: id, Generation: instance.Generation}); err != nil {
			return err
		}

		if _, err := queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: id, Tools: encoded}); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, id)
	}))
}

// markFailed fails a pending instance and notifies.
func (f *mcpFixture) markFailed(t *testing.T, id, reason string) {
	t.Helper()

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := db.New(tx).SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: id, Reason: reason, FromStatus: "pending"}); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, id)
	}))
}

func (f *mcpFixture) enabledServers(t *testing.T, principal, session string) []string {
	t.Helper()

	rows, err := db.New(f.pool).ListSessionServers(t.Context(), f.sessionRow(t, principal, session).ID)
	require.NoError(t, err)

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.TemplateName)
	}

	return names
}

// auditEvents returns the details of the audit rows of eventType for server.
func (f *mcpFixture) auditEvents(t *testing.T, eventType, server string) []map[string]any {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), "SELECT details FROM audit_events WHERE type = $1 AND server = $2 ORDER BY id", eventType, server)
	require.NoError(t, err)

	var events []map[string]any

	for rows.Next() {
		var details map[string]any
		require.NoError(t, rows.Scan(&details))
		events = append(events, details)
	}

	require.NoError(t, rows.Err())

	return events
}

func TestEnableServer(t *testing.T) {
	t.Run("a per-session enable gets progress, then list_changed, and tools named server__tool", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-session", anyone)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")

		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "session:alice:session-1")
		assert.Equal(t, "pending", instance.Status)
		assert.Equal(t, "tg-"+instance.ID, instance.Namespace)
		assert.Len(t, instance.ID, 32)

		require.Eventually(t, func() bool { return len(client.progressMessages()) > 0 }, waitTimeout, 10*time.Millisecond)
		assert.Zero(t, client.listChangedCount(), "list_changed before the instance is ready")

		fixture.markReady(t, instance.ID, "search", "create_issue")

		result := awaitEnable(t, results)
		require.Empty(t, result.err)
		assert.Equal(t, []string{"github__create_issue", "github__search"}, result.tools)

		require.Eventually(t, func() bool { return client.listChangedCount() > 0 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__create_issue", "github__search"), toolNames(t, client.session))

		events := fixture.auditEvents(t, "server.enable", "github")
		require.Len(t, events, 1)
		assert.Equal(t, "allow", events[0]["decision"])
	})

	t.Run("a namespace: shared template's instance is in the configured shared namespace", func(t *testing.T) {
		fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.SharedNamespace = "toolgate-shared" })
		template := fixture.addEnableTemplate("shared-ns", "shared", anyone)
		template.Spec.Namespace = "shared"
		fixture.catalog.Set(template, nil)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "shared-ns")

		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "shared-ns"), "shared")
		assert.Equal(t, "toolgate-shared", instance.Namespace, "not a tg-<id> namespace")

		fixture.markReady(t, instance.ID, "search")
		require.Empty(t, awaitEnable(t, results).err)
	})

	t.Run("a missing secret fails naming the secret and the command, and writes nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
			`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"stringData":{"token":"${secret:github-token}"}}`,
		)}}
		fixture.catalog.Set(template, nil)

		client := fixture.connectObserved(t, alice())
		result := awaitEnable(t, startEnable(t, client.session, "github"))

		assert.Equal(t, "Secret `github-token` isn't set. Run `toolgate secret set github-token`.", result.err)
		assert.Empty(t, fixture.instancesOf(t, template))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.auditEvents(t, "server.enable", "github"))
	})

	t.Run("a remote server with its header secret missing is refused with the set-secret message and starts nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addRemoteTemplate("mail-archiver")

		client := fixture.connectObserved(t, alice())
		result := awaitEnable(t, startEnable(t, client.session, "mail-archiver"))

		assert.Equal(t, "Secret `mail-archiver-api-key` isn't set. Run `toolgate secret set mail-archiver-api-key`.", result.err)
		assert.Empty(t, fixture.instancesOf(t, template))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("a set secret lets the enable go ahead", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
			`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"stringData":{"token":"${secret:github-token}"}}`,
		)}}
		fixture.catalog.Set(template, nil)

		_, err := db.New(fixture.pool).PutUserSecret(t.Context(), db.PutUserSecretParams{
			Principal: "alice", Name: "github-token", Ciphertext: []byte("sealed"), KeyID: 1,
		})
		require.NoError(t, err)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "search")

		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, results))
	})

	t.Run("a refused enable names the rule and is audited", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		require.NoError(t, fixture.rules.Set(blockServer("github")))

		client := fixture.connectObserved(t, alice())
		result := awaitEnable(t, startEnable(t, client.session, "github"))

		assert.Contains(t, result.err, "GitHub is frozen")
		assert.Empty(t, fixture.instancesOf(t, template))

		events := fixture.auditEvents(t, "server.enable", "github")
		require.Len(t, events, 1)
		assert.Equal(t, "refuse", events[0]["decision"])
		assert.Equal(t, templateNamespace+"/block-github", events[0]["rule"])
	})

	t.Run("a failed instance returns its reason and leaves the server enabled", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markFailed(t, instance.ID, "Image `example/server:1` can't be pulled.")

		result := awaitEnable(t, results)
		assert.Contains(t, result.err, "Image `example/server:1` can't be pulled.")
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("a 33rd server is refused", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		row := fixture.sessionRow(t, "alice", "session-1")

		for index := range gateway.MaxEnabledServers {
			require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
				SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "other-" + string(rune('a'+index/26)) + string(rune('a'+index%26)),
			}))
		}

		result := awaitEnable(t, startEnable(t, client.session, "github"))
		assert.Equal(t, "This session already has 32 servers enabled. Disable one with `toolgate_disable_server` first.", result.err)
	})

	t.Run("the 32nd server is allowed", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		row := fixture.sessionRow(t, "alice", "session-1")

		for index := range gateway.MaxEnabledServers - 1 {
			require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
				SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "other-" + string(rune('a'+index/26)) + string(rune('a'+index%26)),
			}))
		}

		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "search")
		assert.Empty(t, awaitEnable(t, results).err)
	})
}

// testStartTimeout is the startTimeout of the templates enabled in these
// tests. A waiting enable gives up after it, so a test whose enable never
// returns fails instead of blocking the server's shutdown.
const testStartTimeout = 15 * time.Second

func (f *mcpFixture) addEnableTemplate(name, scope string, access v1alpha1.Access) *v1alpha1.MCPServerTemplate {
	template := f.addTemplate(name, scope, access)
	template.Spec.StartTimeout = &metav1.Duration{Duration: testStartTimeout}
	f.catalog.Set(template, nil)

	return template
}

// addRemoteTemplate offers a valid per-user remote template whose header
// names a secret alice hasn't set.
func (f *mcpFixture) addRemoteTemplate(name string) *v1alpha1.MCPServerTemplate {
	template := f.addEnableTemplate(name, "per-user", anyone)
	template.Spec.Endpoint = v1alpha1.Endpoint{}
	template.Spec.Resources = nil
	template.Spec.Remote = &v1alpha1.RemoteServer{
		URL:     "http://" + name + ".email.svc.cluster.local:5000/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${secret:" + name + "-api-key}"},
	}
	template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "echo"}}
	f.catalog.Set(template, nil)

	return template
}

func blockServer(server string) *v1alpha1.ToolgateRule {
	return &v1alpha1.ToolgateRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: "block-" + server},
		Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Servers: []string{server}}, Reason: "GitHub is frozen."},
	}
}

func (f *mcpFixture) catalogTemplate(t *testing.T, name string) *v1alpha1.MCPServerTemplate {
	t.Helper()

	template, err := f.catalog.Offered(name)
	require.NoError(t, err)

	return template
}

func TestEnableInteractions(t *testing.T) {
	t.Run("two sessions of one user enabling a per-user server at once share one instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		first := fixture.connectObserved(t, alice())
		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)
		other := fixture.connectObserved(t, bob())

		firstResults := startEnable(t, first.session, "github")
		secondResults := startEnable(t, second.session, "github")
		otherResults := startEnable(t, other.session, "github")

		instance := fixture.awaitInstance(t, template, "user:alice")
		bobInstance := fixture.awaitInstance(t, template, "user:bob")

		// Both of alice's enables have committed before the instance is
		// ready, so neither can have inserted a second row.
		require.Eventually(t, func() bool {
			return len(fixture.auditEvents(t, "server.enable", "github")) == 3
		}, waitTimeout, 10*time.Millisecond)

		fixture.markReady(t, instance.ID, "search")
		fixture.markReady(t, bobInstance.ID, "search")

		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, firstResults))
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, secondResults))
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, otherResults))

		var aliceRows int

		for _, row := range fixture.instancesOf(t, template) {
			if row.ScopeKey == "user:alice" {
				aliceRows++
			}
		}

		assert.Equal(t, 1, aliceRows)
		assert.NotEqual(t, instance.ID, bobInstance.ID)
	})

	t.Run("the same session enabling twice waits on one instance and both get the same result", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)

		client := fixture.connectObserved(t, alice())
		firstResults := startEnable(t, client.session, "github")
		secondResults := startEnable(t, client.session, "github")

		instance := fixture.awaitInstance(t, template, "session:alice:session-1")

		require.Eventually(t, func() bool {
			return len(fixture.auditEvents(t, "server.enable", "github")) == 2
		}, waitTimeout, 10*time.Millisecond)

		fixture.markReady(t, instance.ID, "search")

		first, second := awaitEnable(t, firstResults), awaitEnable(t, secondResults)
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, first)
		assert.Equal(t, first, second)
		assert.Len(t, fixture.instancesOf(t, template), 1)
	})

	t.Run("a disable while an enable waits makes the enable return disabled while starting", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		fixture.awaitInstance(t, template, "session:alice:session-1")

		other := fixture.connectObserved(t, alice())
		disable(t, other.session, "github")

		assert.Equal(t, enableResult{err: "`github` was disabled while starting."}, awaitEnable(t, results))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
	})

	for _, test := range []struct {
		scope    string
		scopeKey string
		// other is a live session that enables the same server and shares
		// the instance; none for per-session.
		other *user
	}{
		{scope: "per-session", scopeKey: "session:alice:session-1"},
		{scope: "per-user", scopeKey: "user:alice", other: &user{claims: alice().claims, session: "session-2"}},
		{scope: "shared", scopeKey: "shared", other: new(bob())},
	} {
		t.Run("a session that ends while an enable waits ends the wait and keeps the server, "+test.scope, func(t *testing.T) {
			fixture := newMCPFixture(t)
			template := fixture.addEnableTemplate("github", test.scope, anyone)

			client := fixture.connectObserved(t, alice())
			results := startEnable(t, client.session, "github")

			var otherResults <-chan enableResult
			if test.other != nil {
				otherResults = startEnable(t, fixture.connectObserved(t, *test.other).session, "github")
			}

			instance := fixture.awaitInstance(t, template, test.scopeKey)

			ended := fixture.sessionRow(t, "alice", "session-1")
			require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE id = $1", ended.ID); err != nil {
					return err
				}

				return store.Notify(t.Context(), tx, store.KindSession, fmt.Sprint(ended.ID))
			}))

			assert.Equal(t, enableResult{err: "This session ended while waiting for `github`."}, awaitEnable(t, results))
			assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"),
				"the ended session keeps the server in its enabled set")
			assert.True(t, fixture.sessionRow(t, "alice", "session-1").EndedAt.Valid, "the wait doesn't restart the session")

			if otherResults != nil {
				// The other live session keeps the instance and its wait.
				fixture.markReady(t, instance.ID, "search")
				assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, otherResults))
			}
		})
	}

	t.Run("a rule added while an enable waits refuses it when the instance is ready", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")

		require.NoError(t, fixture.rules.Set(blockServer("github")))
		fixture.markReady(t, instance.ID, "search")

		result := awaitEnable(t, results)
		assert.Contains(t, result.err, "GitHub is frozen")
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		events := fixture.auditEvents(t, "server.disable", "github")
		require.Len(t, events, 1)
		assert.Equal(t, templateNamespace+"/block-github", events[0]["rule"])
	})
}

func TestDisableServer(t *testing.T) {
	t.Run("disable removes the tools and sends list_changed to that session only", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		bystander := fixture.connectObserved(t, bob())

		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "search")
		require.Empty(t, awaitEnable(t, results).err)
		require.Contains(t, toolNames(t, client.session), "github__search")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		disable(t, client.session, "github")

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Len(t, fixture.auditEvents(t, "server.disable", "github"), 1)
		assert.Zero(t, bystander.listChangedCount())

		// The reconciler decides stops: disable never touches desired.
		row, err := db.New(fixture.pool).GetInstance(t.Context(), instance.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", row.Desired)
	})

	t.Run("disabling a server that isn't enabled succeeds and changes nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		disable(t, client.session, "github")

		assert.Empty(t, fixture.auditEvents(t, "server.disable", "github"))
	})
}

func TestProxiedToolList(t *testing.T) {
	t.Run("tools/list leaves out tools the policy refuses and servers without a ready instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		github := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.addEnableTemplate("jira", "per-user", anyone)
		require.NoError(t, fixture.rules.Set(&v1alpha1.ToolgateRule{
			ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: "no-delete"},
			Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Tools: []string{"github__delete_repo"}}, Reason: "No deletes."},
		}))

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, github, "user:alice")
		fixture.markReady(t, instance.ID, "search", "delete_repo")
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, results))

		// jira is enabled but its instance is still pending.
		_ = startEnable(t, client.session, "jira")
		fixture.awaitInstance(t, fixture.catalogTemplate(t, "jira"), "user:alice")

		assert.ElementsMatch(t, append(metaToolNames, "github__search"), toolNames(t, client.session))

		// Another session of the same user sees the same list without
		// enabling anything itself only when it has the server enabled.
		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, second.session))
	})
}

// serverStatus calls toolgate_server_status for name.
func serverStatus(t *testing.T, session *mcp.ClientSession, name string) gateway.ServerStatusResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: gateway.ToolServerStatus, Arguments: map[string]any{"name": name},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "server_status failed: %v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var status gateway.ServerStatusResult
	require.NoError(t, json.Unmarshal(encoded, &status))

	return status
}

func TestTemplateChange(t *testing.T) {
	t.Run("new instances use the edited template and server_status shows the running one as outdated", func(t *testing.T) {
		fixture := newMCPFixture(t)
		original := fixture.addEnableTemplate("github", "per-session", anyone)

		first := fixture.connectObserved(t, alice())
		results := startEnable(t, first.session, "github")
		running := fixture.awaitInstance(t, original, "session:alice:session-1")
		assert.Equal(t, original.Generation, running.Generation)
		fixture.markReady(t, running.ID, "search")
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, results))

		before := serverStatus(t, first.session, "github")
		assert.Equal(t, gateway.StatusReady, before.Status)
		assert.False(t, before.Outdated, "an instance of the template's current generation isn't outdated")

		edited := original.DeepCopy()
		edited.Generation = original.Generation + 1
		edited.Spec.Description = "The edited github server."
		fixture.catalog.Set(edited, nil)

		after := serverStatus(t, first.session, "github")
		assert.Equal(t, gateway.StatusReady, after.Status, "the running instance keeps running")
		assert.True(t, after.Outdated, "the running instance is older than its template")

		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)
		secondResults := startEnable(t, second.session, "github")
		started := fixture.awaitInstance(t, edited, "session:alice:session-2")
		assert.Equal(t, edited.Generation, started.Generation, "a new instance uses the new generation")
		fixture.markReady(t, started.ID, "search")
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, secondResults))

		assert.False(t, serverStatus(t, second.session, "github").Outdated, "the new instance isn't outdated")
		assert.True(t, serverStatus(t, first.session, "github").Outdated, "the older instance stays outdated")

		current, err := db.New(fixture.pool).GetInstance(t.Context(), running.ID)
		require.NoError(t, err)
		assert.Equal(t, original.Generation, current.Generation, "the running instance keeps its generation")
		assert.Equal(t, "ready", current.Status)
	})
}
