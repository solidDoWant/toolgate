package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// BuiltinServerName is the name of the built-in server, whose tools manage
// the caller's own workspace scope.
const BuiltinServerName = catalog.ReservedServerName

// builtinUID is the UID of the built-in server's in-memory template. No
// Kubernetes object has it, so its search rows never collide with a real
// template's.
const builtinUID = "toolgate-builtin-server"

// Upstream names of the built-in server's tools.
const (
	builtinListScopes = "list_scopes"
	builtinAllow      = "allow"
	builtinDeny       = "deny"
)

// MaxScopeToolItems is the most servers and tools one toolgate_allow or
// toolgate_deny call names.
const MaxScopeToolItems = 32

const builtinScopeSchema = `{"type":"object","properties":{` +
	`"workspace":{"type":"string","description":"A workspace ID, such as coder/alice/dev."},` +
	`"session":{"type":"string","description":"The session ID of one of your open sessions."},` +
	`"servers":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":32,` +
	`"description":"Server names, or <server>__<tool> for one tool."}},` +
	`"required":["servers"]}`

// builtinToolPrefix starts the proxied name of a built-in tool: the tools
// are offered as `toolgate_list_scopes`, `toolgate_allow` and
// `toolgate_deny`, the only proxied names without the separator.
const builtinToolPrefix = BuiltinServerName + "_"

// builtinTools are the upstream names the built-in server's proxied names
// map to.
var builtinTools = map[string]bool{builtinListScopes: true, builtinAllow: true, builtinDeny: true}

// BuiltinTemplate returns the in-memory template of the built-in server.
// Every identity of kind user has access, from any environment. It has no
// resources and never has an instance. The catalog holds it only on a
// gateway with the web UI.
func BuiltinTemplate() *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{}
	template.Name = BuiltinServerName
	template.UID = builtinUID
	template.Generation = 1
	template.Spec = v1alpha1.MCPServerTemplateSpec{
		Description: "Your own workspace scope: list what each workspace and session may use, and allow or deny servers or tools in them.",
		Scope:       catalog.ScopeShared,
		Start:       catalog.StartOnCall,
		Access:      v1alpha1.Access{AllUsers: true, Environments: policy.EnvironmentAny},
		DeclaredTools: []v1alpha1.DeclaredTool{{
			Name: builtinListScopes,
			Description: "List your own workspace scope: your defaults, each workspace's allowed servers and tools and " +
				"its denied ones, and each open session's additions and denials.",
			InputSchema: &runtime.RawExtension{Raw: []byte(`{"type":"object"}`)},
		}, {
			Name: builtinAllow,
			Description: "Ask to allow servers or tools in one of your workspaces or open sessions with a verified workspace. " +
				"Name exactly one of `workspace` and `session`, and 1 to 32 items: a server name, or `<server>__<tool>` for one tool. " +
				"Nothing changes until you approve the change in your browser, at the link this tool returns.",
			InputSchema: &runtime.RawExtension{Raw: []byte(builtinScopeSchema)},
		}, {
			Name: builtinDeny,
			Description: "Deny servers or tools in one of your workspaces or open sessions, at once. Name exactly one of " +
				"`workspace` and `session`, and 1 to 32 items: a server name, or `<server>__<tool>` for one tool.",
			InputSchema: &runtime.RawExtension{Raw: []byte(builtinScopeSchema)},
		}},
	}

	return template
}

// isBuiltin reports whether template is the built-in server's.
func isBuiltin(template *v1alpha1.MCPServerTemplate) bool {
	return template != nil && template.Name == BuiltinServerName && string(template.UID) == builtinUID
}

// builtinProxiedName returns the proxied name of a built-in tool, and false
// for any other server or tool.
func builtinProxiedName(server, tool string) (string, bool) {
	if server != BuiltinServerName || !builtinTools[tool] {
		return "", false
	}

	return builtinToolPrefix + tool, true
}

// splitProxiedName splits a proxied tool name into its server and its
// exposed tool name. The built-in tools' exact names belong to server
// `toolgate`. ok is false for a name that isn't a proxied name.
func splitProxiedName(name string) (server, tool string, ok bool) {
	if len(name) > len(builtinToolPrefix) && name[:len(builtinToolPrefix)] == builtinToolPrefix {
		if upstream := name[len(builtinToolPrefix):]; builtinTools[upstream] {
			return BuiltinServerName, upstream, true
		}
	}

	server, tool, ok = cutProxiedName(name)

	return server, tool, ok
}

// ListScopesResult is the toolgate_list_scopes result.
type ListScopesResult struct {
	Current CurrentScope `json:"current"`
	ScopeSummary
}

// CurrentScope is the calling session.
type CurrentScope struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Verified  bool   `json:"verified"`
}

// callBuiltin runs an allowed call of a built-in tool for the caller's own
// identity, through the same methods as the API, and returns its result
// and outcome as forward does. A non-nil error is a cancel or a disconnect
// while toolgate_allow waits, which writes no result.
func (m *metaTools) callBuiltin(ctx context.Context, current requestContext, request *mcp.CallToolRequest, tool string, arguments json.RawMessage) (*mcp.CallToolResult, string, error) {
	switch tool {
	case builtinListScopes:
		result, outcome := m.listScopes(ctx, current)

		return result, outcome, nil
	case builtinAllow:
		return m.allowScope(ctx, current, request, arguments)
	case builtinDeny:
		result, outcome := m.denyScope(ctx, current, arguments)

		return result, outcome, nil
	default:
		return errorResult(fmt.Sprintf("Server `%s` has no tool `%s`.", BuiltinServerName, tool)), "error", nil
	}
}

func (m *metaTools) listScopes(ctx context.Context, current requestContext) (*mcp.CallToolResult, string) {
	summary, err := m.workspaces.ScopeSummary(ctx, current.identity.Principal, m.sessionIdleTimeout)
	if err != nil {
		return errorResult("The gateway couldn't read your scope. Try again later."), "failed"
	}

	result := ListScopesResult{
		Current: CurrentScope{
			Session: current.identity.Session, Workspace: current.identity.Workspace,
			Verified: VerifiedWorkspace(current.identity) != "",
		},
		ScopeSummary: summary,
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return errorResult("The gateway couldn't encode your scope."), "failed"
	}

	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: json.RawMessage(encoded),
	}, "ok"
}

// scopeToolInput is the input of toolgate_allow and toolgate_deny.
type scopeToolInput struct {
	Workspace string   `json:"workspace"`
	Session   string   `json:"session"`
	Servers   []string `json:"servers"`
}

// denyScope runs toolgate_deny: the same deny as the API and the CLI, for
// the caller's own identity.
func (m *metaTools) denyScope(ctx context.Context, current requestContext, arguments json.RawMessage) (*mcp.CallToolResult, string) {
	input, refusal := parseScopeToolInput(arguments)
	if refusal != "" {
		return errorResult(refusal), "error"
	}

	change := WorkspaceChange{Action: WorkspaceDeny, Workspace: input.Workspace, Session: input.Session, Servers: input.Servers}

	err := m.workspaces.ApplyNarrowing(ctx, current.identity, change, ViaTool)

	var refused *WorkspaceError

	switch {
	case errors.As(err, &refused):
		return errorResult(refused.Message), "error"
	case err != nil:
		return errorResult("The gateway couldn't apply the deny. Try again later."), "failed"
	}

	target := fmt.Sprintf("workspace `%s`", input.Workspace)
	if input.Session != "" {
		target = fmt.Sprintf("session `%s`", input.Session)
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Denied %s in %s.", strings.Join(change.Servers, ", "), target)}},
	}, "ok"
}
