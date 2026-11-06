package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// toolSeparator joins a server name and an upstream tool name into the name
// a session sees.
const toolSeparator = "__"

// ProxiedToolName is the name a session sees for tool of server.
func ProxiedToolName(server, tool string) string {
	return server + toolSeparator + tool
}

// sessionTools is the set of proxied tools one MCP session's server
// currently offers. Each MCP session has its own mcp.Server, so changing the
// set sends `notifications/tools/list_changed` to that session only.
type sessionTools struct {
	server *mcp.Server
	// call answers a call of any proxied tool.
	call mcp.ToolHandler
	// current is the identity and session of the latest request on this MCP
	// session. Fan-out syncs the session with it.
	current requestContext

	mu sync.Mutex
	// offered maps each proxied tool name to its JSON definition.
	offered map[string][]byte
}

func newSessionTools(server *mcp.Server, call mcp.ToolHandler, current requestContext) *sessionTools {
	return &sessionTools{server: server, call: call, current: current, offered: make(map[string][]byte)}
}

func (s *sessionTools) setCurrent(current requestContext) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.current = current
}

func (s *sessionTools) currentRequest() requestContext {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.current
}

// apply makes the server offer exactly wanted. Unchanged tools are left
// alone, so a sync that changes nothing sends no list_changed.
func (s *sessionTools) apply(wanted map[string]*mcp.Tool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []string

	for name := range s.offered {
		if _, keep := wanted[name]; !keep {
			removed = append(removed, name)
			delete(s.offered, name)
		}
	}

	if len(removed) > 0 {
		s.server.RemoveTools(removed...)
	}

	for name, tool := range wanted {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return fmt.Errorf("encode tool %s: %w", name, err)
		}

		if bytes.Equal(s.offered[name], encoded) {
			continue
		}

		s.server.AddTool(tool, s.call)
		s.offered[name] = encoded
	}

	return nil
}

// proxiedTools returns the proxied tools the session should see, keyed by
// their names: for each enabled server with a ready live instance, its tools
// minus those the policy refuses for this identity. byServer lists the names
// for each server.
func (m *metaTools) proxiedTools(ctx context.Context, current requestContext) (tools map[string]*mcp.Tool, byServer map[string][]string, err error) {
	queries := db.New(m.database)

	rows, err := queries.ListSessionServers(ctx, current.session.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("read enabled servers: %w", err)
	}

	tools = make(map[string]*mcp.Tool)
	byServer = make(map[string][]string)

	if len(rows) == 0 {
		return tools, byServer, nil
	}

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, nil, err
	}

	for _, row := range rows {
		template, found := m.catalog.Running(types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName})
		if !found {
			continue
		}

		if !m.policy.Decide(policy.Request{Identity: current.identity, Template: template, State: state}).Allowed {
			continue
		}

		instance, err := queries.GetLiveInstance(ctx, db.GetLiveInstanceParams{
			TemplateNs: template.Namespace, TemplateName: template.Name,
			ScopeKey: scopeKey(template.Spec.Scope, current.identity),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}

		if err != nil {
			return nil, nil, fmt.Errorf("read live instance: %w", err)
		}

		// An unhealthy instance's tools are withdrawn until it is healthy
		// again.
		if instance.Status != statusReady || instance.UnhealthySince.Valid {
			continue
		}

		var upstream []*mcp.Tool
		if err := json.Unmarshal(instance.Tools, &upstream); err != nil {
			return nil, nil, fmt.Errorf("decode tools of instance %s: %w", instance.ID, err)
		}

		for _, curated := range toolcatalog.Curate(template, upstream) {
			request := policy.Request{
				Identity: current.identity, Template: template, Tool: curated.UpstreamName,
				ExposedTool: curated.ExposedName, State: state,
			}
			if !m.policy.Decide(request).Allowed {
				continue
			}

			proxied := *curated.Tool
			proxied.Name = ProxiedToolName(template.Name, curated.ExposedName)

			if proxied.InputSchema == nil {
				proxied.InputSchema = map[string]any{"type": "object"}
			}

			tools[proxied.Name] = &proxied
			byServer[template.Name] = append(byServer[template.Name], proxied.Name)
		}

		sort.Strings(byServer[template.Name])
	}

	return tools, byServer, nil
}

// syncSession brings the proxied tools of the MCP session session up to
// date for current, and returns the proxied tool names of each server.
func (m *metaTools) syncSession(ctx context.Context, session *mcp.ServerSession, current requestContext) (map[string][]string, error) {
	tools, byServer, err := m.proxiedTools(ctx, current)
	if err != nil {
		return nil, err
	}

	if session == nil {
		return byServer, nil
	}

	offered, found := m.sessionTools(session.ID())
	if !found {
		return byServer, nil
	}

	if err := offered.apply(tools); err != nil {
		return nil, err
	}

	return byServer, nil
}

// syncOnList is receiving middleware that brings the session's proxied tools
// up to date before the go-sdk answers `tools/list`.
func (m *metaTools) syncOnList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method == "tools/list" {
			session, _ := request.GetSession().(*mcp.ServerSession)

			if extra := request.GetExtra(); extra != nil && extra.TokenInfo != nil {
				if current, ok := extra.TokenInfo.Extra[requestContextKey].(requestContext); ok {
					if _, err := m.syncSession(ctx, session, current); err != nil {
						return nil, err
					}
				}
			}
		}

		return next(ctx, method, request)
	}
}

// routeProxiedCalls sends every `tools/call` of a `<server>__<tool>` name to
// callTool, also when the session doesn't offer that tool (not yet, or no
// longer), so every such call gets a recorded decision. Meta tool names
// have no separator. A tool the session offers goes through the go-sdk's
// own tool path, whose multi round-trip handling serves URL elicitation; a
// call of a tool it doesn't offer is answered here without elicitation.
func (m *metaTools) routeProxiedCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}

		call, ok := request.(*mcp.CallToolRequest)
		if !ok || call.Params == nil || !strings.Contains(call.Params.Name, toolSeparator) {
			return next(ctx, method, request)
		}

		if m.offers(call.Session, call.Params.Name) {
			result, err := next(ctx, method, request)

			var wireErr *jsonrpc.Error
			if !errors.As(err, &wireErr) || wireErr.Code != jsonrpc.CodeInvalidParams || !strings.HasPrefix(wireErr.Message, "unknown tool") {
				return result, err
			}

			// The tool was removed after the check above.
		}

		return m.callTool(withoutElicitation(ctx), call)
	}
}

// offers reports whether the MCP session currently offers the proxied tool
// name.
func (m *metaTools) offers(session *mcp.ServerSession, name string) bool {
	if session == nil || m.sessionTools == nil {
		return false
	}

	offered, found := m.sessionTools(session.ID())
	if !found {
		return false
	}

	offered.mu.Lock()
	defer offered.mu.Unlock()

	_, ok := offered.offered[name]

	return ok
}

type noElicitationKey struct{}

// withoutElicitation marks a call answered outside the go-sdk's tool path,
// where an input request would not reach the client.
func withoutElicitation(ctx context.Context) context.Context {
	return context.WithValue(ctx, noElicitationKey{}, true)
}

func elicitationAllowed(ctx context.Context) bool {
	disabled, _ := ctx.Value(noElicitationKey{}).(bool)

	return !disabled
}
