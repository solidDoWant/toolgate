package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// BuiltinServerName is the name of the built-in server, whose tools manage
// the caller's own workspace scope (docs/design/phase-4.md §7).
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
		Description: "Your own workspace scope: list what each workspace and session may use.",
		Scope:       catalog.ScopeShared,
		Start:       catalog.StartOnCall,
		Access:      v1alpha1.Access{AllUsers: true, Environments: policy.EnvironmentAny},
		DeclaredTools: []v1alpha1.DeclaredTool{{
			Name: builtinListScopes,
			Description: "List your own workspace scope: your defaults, each workspace's allowed servers and tools and " +
				"its denied ones, and each open session's additions and denials.",
			InputSchema: &runtime.RawExtension{Raw: []byte(`{"type":"object"}`)},
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
// and outcome as forward does.
func (m *metaTools) callBuiltin(ctx context.Context, current requestContext, tool string) (*mcp.CallToolResult, string) {
	switch tool {
	case builtinListScopes:
		return m.listScopes(ctx, current)
	default:
		return errorResult(fmt.Sprintf("Server `%s` has no tool `%s`.", BuiltinServerName, tool)), "error"
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
