//go:build integration

package gateway_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	toolruntime "github.com/solidDoWant/toolgate/internal/runtime"
)

// declareTools gives template a declared tool list and rebuilds its search
// rows, as the leader does.
func (f *mcpFixture) declareTools(t *testing.T, template *v1alpha1.MCPServerTemplate, tools ...v1alpha1.DeclaredTool) {
	t.Helper()

	template.Spec.DeclaredTools = tools
	f.catalog.Set(template, nil)

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		return toolruntime.RebuildToolSearch(t.Context(), tx, string(template.UID), template)
	}))
}

// listServersInWorkspace gives alice's list for workspace the servers.
func (f *mcpFixture) listServersInWorkspace(t *testing.T, workspace string, servers ...string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `WITH scope AS (
			INSERT INTO user_scopes (principal) VALUES ('alice')
			ON CONFLICT (principal) DO UPDATE SET principal = EXCLUDED.principal RETURNING id),
		list AS (INSERT INTO workspace_lists (scope_id, workspace) SELECT id, $1 FROM scope RETURNING id)
		INSERT INTO workspace_list_servers (list_id, server)
		SELECT list.id, unnest($2::text[]) FROM list`, workspace, servers)
	require.NoError(t, err)
}

func declared(name, description string) v1alpha1.DeclaredTool {
	return v1alpha1.DeclaredTool{Name: name, Description: description, InputSchema: &runtime.RawExtension{Raw: []byte(`{"type":"object"}`)}}
}

func searching(client user) user {
	client.search = "on"

	return client
}

// findTools calls toolgate_find_tools and decodes its result.
func findTools(t *testing.T, session *mcp.ClientSession, query string, limit int) (gateway.FindToolsResult, *mcp.CallToolResult) {
	t.Helper()

	arguments := map[string]any{"query": query}
	if limit > 0 {
		arguments["limit"] = limit
	}

	result := callTool(t, session, gateway.ToolFindTools, arguments)

	var found gateway.FindToolsResult

	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(encoded, &found))
	}

	return found, result
}

func foundNames(found gateway.FindToolsResult) []string {
	names := make([]string, 0, len(found.Tools))
	for _, tool := range found.Tools {
		names = append(names, tool.Name)
	}

	return names
}

func (f *mcpFixture) foundToolCount(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_found_tools`).Scan(&count))

	return count
}

func TestToolSearchMode(t *testing.T) {
	t.Run("row 230: the header and the setting decide the mode on each request, and find_tools is only listed in search mode", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, template, declared("create_issue", "Create an issue"))

		off := fixture.connect(t, alice())
		assert.ElementsMatch(t, metaToolNames, toolNames(t, off))

		result := callTool(t, off, gateway.ToolFindTools, map[string]any{"query": "issue"})
		require.True(t, result.IsError)
		assert.Equal(t, "Tool search is off for this session.", resultText(result))

		on := fixture.connect(t, searching(alice()))
		assert.ElementsMatch(t, append([]string{gateway.ToolFindTools}, metaToolNames...), toolNames(t, on))

		explicitOff := alice()
		explicitOff.search = "off"
		assert.ElementsMatch(t, metaToolNames, toolNames(t, fixture.connect(t, explicitOff)))
	})

	t.Run("the setting turns search mode on when no header is sent, and the header wins", func(t *testing.T) {
		fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.ToolSearch = true })

		assert.ElementsMatch(t, append([]string{gateway.ToolFindTools}, metaToolNames...), toolNames(t, fixture.connect(t, alice())))

		explicitOff := alice()
		explicitOff.search = "off"
		assert.ElementsMatch(t, metaToolNames, toolNames(t, fixture.connect(t, explicitOff)))
	})

	t.Run("a search returns tools with schemas, adds them to the list, and a call enables the server", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, template, declared("echo", "Echo text back to the caller"), declared("list_repos", "List repositories"))
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, searching(alice()))
		before := toolNames(t, client.session)

		found, _ := findTools(t, client.session, "echo text", 0)
		require.NotEmpty(t, found.Tools)
		assert.Equal(t, "github__echo", found.Tools[0].Name)
		assert.Equal(t, "github", found.Tools[0].Server)
		assert.Equal(t, accessAllowedValue, found.Tools[0].Access)
		assert.Equal(t, "Echo text back to the caller", found.Tools[0].Description)
		assert.Equal(t, map[string]any{"type": "object"}, found.Tools[0].InputSchema)

		assert.Len(t, toolNames(t, client.session), len(before)+len(found.Tools))
		assert.Contains(t, toolNames(t, client.session), "github__echo")

		require.Eventually(t, func() bool { return client.listChangedCount() > 0 }, waitTimeout, 20*time.Millisecond)

		assert.Equal(t, 0, len(fixture.auditRows(t, "tool.decision")), "a search is not an audit event")

		type outcome struct {
			result *mcp.CallToolResult
			err    error
		}

		outcomes := make(chan outcome, 1)

		go func() {
			result, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__echo", Arguments: map[string]any{"text": "x"}})
			outcomes <- outcome{result, err}
		}()

		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "echo")

		call := <-outcomes
		require.NoError(t, call.err)
		require.False(t, call.result.IsError, resultText(call.result))
		assert.Len(t, fixture.auditRows(t, "server.enable"), 1)
		assert.Equal(t, "first-call", fixture.auditRows(t, "server.enable")[0].Details["reason"])
	})

	t.Run("row 252: a query with no words, or only stop words, gives no tools and the message", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, template, declared("create_issue", "Create an issue"))

		client := fixture.connect(t, searching(alice()))

		for _, query := range []string{"создать задачу", "the and of", "!!!"} {
			found, result := findTools(t, client, query, 0)
			assert.Empty(t, found.Tools, query)
			assert.Equal(t, "The query has no words to search for. Describe the task in English words.", resultText(result), query)
		}

		found, result := findTools(t, client, "zzzunknown", 0)
		assert.Empty(t, found.Tools)
		assert.NotContains(t, resultText(result), "no words")
	})

	t.Run("the limits: limit 20 and query 2000 characters pass, one more is refused", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		tools := []v1alpha1.DeclaredTool{}
		for index := range 30 {
			tools = append(tools, declared(fmt.Sprintf("tool_%d", index), "shared words"))
		}

		fixture.declareTools(t, template, tools...)

		client := fixture.connect(t, searching(alice()))

		found, _ := findTools(t, client, "shared", 20)
		assert.Len(t, found.Tools, 20)

		found, _ = findTools(t, client, "shared", 0)
		assert.Len(t, found.Tools, 5)

		assert.True(t, callTool(t, client, gateway.ToolFindTools, map[string]any{"query": "shared", "limit": 21}).IsError)

		long := make([]byte, 2000)
		for index := range long {
			long[index] = 'a'
		}

		assert.False(t, callTool(t, client, gateway.ToolFindTools, map[string]any{"query": string(long)}).IsError)
		assert.True(t, callTool(t, client, gateway.ToolFindTools, map[string]any{"query": string(long) + "a"}).IsError)
		assert.True(t, callTool(t, client, gateway.ToolFindTools, map[string]any{"query": ""}).IsError)
	})

	t.Run("row 228: a 51st found tool pushes out the oldest, row 229: ending the session deletes them", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		tools := []v1alpha1.DeclaredTool{}
		for index := range 51 {
			tools = append(tools, declared(fmt.Sprintf("tool%02d", index), fmt.Sprintf("word%02d", index)))
		}

		fixture.declareTools(t, template, tools...)

		client := fixture.connectObserved(t, searching(alice()))

		for index := range 51 {
			found, _ := findTools(t, client.session, fmt.Sprintf("word%02d", index), 1)
			require.Len(t, found.Tools, 1)
		}

		names := toolNames(t, client.session)
		assert.Equal(t, 50, fixture.foundToolCount(t))
		assert.NotContains(t, names, "github__tool00", "the oldest was pushed out")
		assert.Contains(t, names, "github__tool50")

		fixture.endIdleSession(t, "session-1")
		assert.Equal(t, 0, fixture.foundToolCount(t))
	})

	t.Run("row 231: a rebuild of the template's rows sends list_changed to search-mode sessions with its found tools", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, template, declared("create_issue", "Create an issue"))

		finder := fixture.connectObserved(t, searching(alice()))
		other := fixture.connectObserved(t, searching(bob()))

		found, _ := findTools(t, finder.session, "issue", 0)
		require.Len(t, found.Tools, 1)
		assert.Contains(t, toolNames(t, finder.session), "github__create_issue")
		assert.Contains(t, toolNames(t, other.session), gateway.ToolFindTools)

		changesBefore := finder.listChangedCount()
		otherBefore := other.listChangedCount()

		// The tool's description changes, as after a new learned list.
		template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{declared("create_issue", "Open a new issue")}
		fixture.catalog.Set(template, nil)
		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			return toolruntime.RebuildToolSearch(t.Context(), tx, string(template.UID), template)
		}))

		require.Eventually(t, func() bool { return finder.listChangedCount() > changesBefore }, waitTimeout, 20*time.Millisecond)

		list, err := finder.session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		assert.Equal(t, otherBefore, other.listChangedCount(), "a session without found tools of the template gets nothing")

		descriptions := map[string]string{}
		for _, tool := range list.Tools {
			descriptions[tool.Name] = tool.Description
		}

		assert.Equal(t, "Open a new issue", descriptions["github__create_issue"])
	})
}

func TestAC4_02SearchNeverReturnsRefusedTools(t *testing.T) {
	newFixture := func(t *testing.T) *mcpFixture {
		t.Helper()

		fixture := newMCPFixtureWithAuth(t, attestedUsers(), withWorkspaceScoping, withApprovalURL)

		open := fixture.addEnableTemplate("open", "per-user", anyone)
		open.Spec.Tools = map[string]v1alpha1.ToolOverride{"zebra_blocked": {Blocked: true}}
		fixture.declareTools(t, open, declared("zebra_read", "zebra read"), declared("zebra_blocked", "zebra blocked"))

		private := fixture.addEnableTemplate("private", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
		fixture.declareTools(t, private, declared("zebra_private", "zebra private"))

		strict := fixture.addEnableTemplate("strict", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "attested"})
		fixture.declareTools(t, strict, declared("zebra_strict", "zebra strict"))

		outside := fixture.addEnableTemplate("outside", "per-user", anyone)
		fixture.declareTools(t, outside, declared("zebra_outside", "zebra outside"))

		forbidden := fixture.addEnableTemplate("forbidden", "per-user", anyone)
		fixture.declareTools(t, forbidden, declared("zebra_forbidden", "zebra forbidden"))

		fixture.startUpstream(t, "outside")
		fixture.setVerifiedDefault(t, "none")
		fixture.listServersInWorkspace(t, aliceWorkspace, "open", "strict", "forbidden")
		fixture.addWorkspaceEntry(t, aliceWorkspace, "forbidden", "", "deny")

		return fixture
	}

	t.Run("an unverified session gets only what it may use now: no blocked, outside-access, attested-only or outside-the-list tool", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.setUnverifiedList(t, "open")

		client := fixture.connect(t, searching(alice()))

		found, _ := findTools(t, client, "zebra", 20)
		assert.Equal(t, []string{"open__zebra_read"}, foundNames(found))
		assert.Equal(t, accessAllowedValue, found.Tools[0].Access)
	})

	t.Run("a verified workspace gets outside-the-list tools as ask, never denied ones, and the ask tool stays in the list and asks on its first call", func(t *testing.T) {
		fixture := newFixture(t)
		client := fixture.connectElicitingAs(t, searching(attestedAlice()), elicitingOptions{revision: revisionElicitCreate, answer: "decline"})

		found, _ := findTools(t, client.session, "zebra", 20)
		assert.ElementsMatch(t, []string{"open__zebra_read", "outside__zebra_outside", "strict__zebra_strict"}, foundNames(found))

		access := map[string]string{}
		for _, tool := range found.Tools {
			access[tool.Name] = tool.Access
		}

		assert.Equal(t, accessAllowedValue, access["open__zebra_read"])
		assert.Equal(t, accessAskValue, access["outside__zebra_outside"])
		assert.NotContains(t, access, "forbidden__zebra_forbidden")
		assert.NotContains(t, access, "private__zebra_private")
		assert.NotContains(t, access, "open__zebra_blocked")

		assert.Contains(t, toolNames(t, client.session), "outside__zebra_outside", "the ask tool is registered on the MCP server")

		result := callTool(t, client.session, "outside__zebra_outside", map[string]any{})
		require.True(t, result.IsError)

		requests := fixture.awaitServerRequests(t, 1)
		assert.Equal(t, "outside", requests[0].Server)

		elicitations := client.elicitations()
		require.Len(t, elicitations, 1)
		assert.Equal(t, "url", elicitations[0].Mode)
		assert.Equal(t, approvalBaseURL+requests[0].ID, elicitations[0].URL)

		denied := callTool(t, client.session, "forbidden__zebra_forbidden", map[string]any{})
		require.True(t, denied.IsError)
		assert.Len(t, fixture.serverRequests(t), 1, "a deny creates no request")

		decisions := fixture.auditRows(t, "tool.decision")
		require.NotEmpty(t, decisions)
		assert.Equal(t, "denied", decisions[len(decisions)-1].Details["reason"])
	})

	t.Run("row 179, row 232: a rule added after the search refuses the call, and the next list leaves the tool out", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.setUnverifiedList(t, "open")

		client := fixture.connect(t, searching(alice()))
		found, _ := findTools(t, client, "zebra read", 5)
		require.Equal(t, []string{"open__zebra_read"}, foundNames(found))
		require.Contains(t, toolNames(t, client), "open__zebra_read")

		require.NoError(t, fixture.rules.Set(blockServer("open")))

		assert.NotContains(t, toolNames(t, client), "open__zebra_read")

		result := callTool(t, client, "open__zebra_read", map[string]any{})
		assert.True(t, result.IsError)

		fixture.catalog.Delete(types.NamespacedName{Namespace: templateNamespace, Name: "open"})

		result = callTool(t, client, "open__zebra_read", map[string]any{})
		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "has no tool")
	})
}

const (
	accessAllowedValue = "allowed"
	accessAskValue     = "ask"
)

// TestAC4_03SearchQuality runs the frozen fixture through the real rebuild
// and the real search.
func TestAC4_03SearchQuality(t *testing.T) {
	type tool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}

	var servers struct {
		Servers []struct {
			Name  string `json:"name"`
			Tools []tool `json:"tools"`
		} `json:"servers"`
	}

	var tasks struct {
		Tasks []struct {
			ID       string `json:"id"`
			Task     string `json:"task"`
			Expected struct {
				Server string `json:"server"`
				Tool   string `json:"tool"`
			} `json:"expected"`
		} `json:"tasks"`
	}

	dir := filepath.Join("..", "..", "test", "fixtures", "toolsearch")

	for name, target := range map[string]any{"servers.json": &servers, "tasks.json": &tasks} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, target))
	}

	fixture := newMCPFixture(t)

	for _, server := range servers.Servers {
		template := fixture.addEnableTemplate(server.Name, "per-user", anyone)

		declaredTools := make([]v1alpha1.DeclaredTool, 0, len(server.Tools))

		for _, item := range server.Tools {
			schema, err := json.Marshal(item.InputSchema)
			require.NoError(t, err)

			declaredTools = append(declaredTools, v1alpha1.DeclaredTool{Name: item.Name, Description: item.Description, InputSchema: &runtime.RawExtension{Raw: schema}})
		}

		fixture.declareTools(t, template, declaredTools...)
	}

	client := fixture.connect(t, searching(alice()))
	hits := 0

	for _, task := range tasks.Tasks {
		found, _ := findTools(t, client, task.Task, 5)
		expected := gateway.ProxiedToolName(task.Expected.Server, task.Expected.Tool)

		if contains(foundNames(found), expected) {
			hits++
		} else {
			t.Logf("miss %s: wanted %s, got %v", task.ID, expected, foundNames(found))
		}
	}

	rate := float64(hits) / float64(len(tasks.Tasks))
	t.Logf("top-5 hits: %d of %d (%.1f%%)", hits, len(tasks.Tasks), rate*100)
	assert.GreaterOrEqual(t, rate, 0.9)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}
