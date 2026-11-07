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

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// toolSeparator joins a server name and an upstream tool name into the name
// a session sees.
const toolSeparator = "__"

// ProxiedToolName is the name a session sees for tool of server. The
// built-in server's tools have their own exact names.
func ProxiedToolName(server, tool string) string {
	if name, ok := builtinProxiedName(server, tool); ok {
		return name
	}

	return server + toolSeparator + tool
}

// cutProxiedName splits a `<server>__<tool>` name.
func cutProxiedName(name string) (server, tool string, ok bool) {
	return strings.Cut(name, toolSeparator)
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
	// synced says offered was brought up to date from the rows at least
	// once. A copy that another replica's session was adopted into starts
	// unsynced.
	synced bool
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

	s.synced = true

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
		return tools, byServer, m.withFoundTools(ctx, current, tools, byServer)
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

		upstream, found, err := m.listedTools(ctx, current, template)
		if err != nil {
			return nil, nil, err
		}

		if !found {
			continue
		}

		for _, curated := range toolcatalog.Curate(template, upstream) {
			request := toolPolicyRequest(current, template, curated.UpstreamName, curated.ExposedName, state)
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

	return tools, byServer, m.withFoundTools(ctx, current, tools, byServer)
}

// withFoundTools adds the session's found tools in search mode.
func (m *metaTools) withFoundTools(ctx context.Context, current requestContext, tools map[string]*mcp.Tool, byServer map[string][]string) error {
	if !current.searchMode {
		return nil
	}

	if err := m.addFoundTools(ctx, current, tools, byServer); err != nil {
		return err
	}

	for server := range byServer {
		sort.Strings(byServer[server])
	}

	return nil
}

// listedTools returns the upstream tools listed for an enabled server, and
// whether it lists any:
//   - a ready, healthy live instance lists its own tools;
//   - a pending or starting instance, or no live instance, lists the catalog
//     list, unless the newest instance failed on the template's current
//     generation;
//   - an unhealthy instance lists none.
func (m *metaTools) listedTools(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate) ([]*mcp.Tool, bool, error) {
	// The built-in server lists its declared tools.
	if isBuiltin(template) {
		return m.catalogList(ctx, template)
	}

	queries := db.New(m.database)

	instance, err := queries.GetLiveInstance(ctx, db.GetLiveInstanceParams{
		TemplateNs: template.Namespace, TemplateName: template.Name,
		ScopeKey: scopeKey(template.Spec.Scope, current.identity),
	})

	switch {
	case err == nil && instance.Status == statusReady:
		// An unhealthy instance's tools are withdrawn until it is healthy
		// again.
		if instance.UnhealthySince.Valid {
			return nil, false, nil
		}

		var upstream []*mcp.Tool
		if err := json.Unmarshal(instance.Tools, &upstream); err != nil {
			return nil, false, fmt.Errorf("decode tools of instance %s: %w", instance.ID, err)
		}

		return upstream, true, nil
	case err == nil:
		// Pending or starting. An instance that was ready before (it is
		// starting again) or that replaces one still stopping withdraws
		// the server's tools until it is ready, as a repair or a restart
		// always did. A first start lists the catalog list.
		if instance.ReadyAt.Valid {
			return nil, false, nil
		}

		predecessors, err := queries.CountPredecessorInstances(ctx, db.CountPredecessorInstancesParams{
			TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName, ScopeKey: instance.ScopeKey, ID: instance.ID,
		})
		if err != nil {
			return nil, false, fmt.Errorf("read predecessor instances: %w", err)
		}

		if predecessors > 0 {
			return nil, false, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		newest, found, err := m.newestInstance(ctx, current.identity, template)
		if err != nil {
			return nil, false, err
		}

		if found && newest.FailedAt.Valid && newest.Generation == template.Generation && newest.TemplateUid == string(template.UID) {
			return nil, false, nil
		}
	default:
		return nil, false, fmt.Errorf("read live instance: %w", err)
	}

	return m.catalogList(ctx, template)
}

// catalogList returns the upstream tools of the template's catalog list.
func (m *metaTools) catalogList(ctx context.Context, template *v1alpha1.MCPServerTemplate) ([]*mcp.Tool, bool, error) {
	var learned []toolcatalog.Learned

	if len(template.Spec.DeclaredTools) == 0 {
		rows, err := db.New(m.database).ListToolCatalogForTemplate(ctx, string(template.UID))
		if err != nil {
			return nil, false, fmt.Errorf("read the tool catalog: %w", err)
		}

		for _, row := range rows {
			learned = append(learned, toolcatalog.Learned{Generation: row.Generation, Tools: row.Tools})
		}
	}

	return toolcatalog.CatalogList(template, learned)
}

// syncSession brings the proxied tools of the MCP session session up to
// date for current, and returns the proxied tool names of each server.
func (m *metaTools) syncSession(ctx context.Context, session *mcp.ServerSession, current requestContext) (map[string][]string, error) {
	tools, byServer, err := m.proxiedTools(ctx, current)
	if err != nil {
		return nil, err
	}

	offered, found := m.offeredTools(session, current)
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

		result, err := next(ctx, method, request)
		if method == "tools/list" && err == nil {
			if extra := request.GetExtra(); extra != nil && extra.TokenInfo != nil {
				if current, ok := extra.TokenInfo.Extra[requestContextKey].(requestContext); ok && !current.searchMode {
					hideFindTools(result)
				}
			}
		}

		return result, err
	}
}

// hideFindTools removes toolgate_find_tools from a tools/list result: outside
// search mode it isn't listed.
func hideFindTools(result mcp.Result) {
	list, ok := result.(*mcp.ListToolsResult)
	if !ok {
		return
	}

	kept := make([]*mcp.Tool, 0, len(list.Tools))

	for _, tool := range list.Tools {
		if tool.Name != ToolFindTools {
			kept = append(kept, tool)
		}
	}

	list.Tools = kept
}

// routeProxiedCalls sends every `tools/call` of a `<server>__<tool>` name to
// callTool, also when the session doesn't offer that tool (not yet, or no
// longer), so every such call gets a recorded decision. The built-in
// server's exact tool names count as proxied names. Meta tool names have no
// separator. A tool the session offers goes through the go-sdk's
// own tool path, whose multi round-trip handling serves URL elicitation; a
// call of a tool it doesn't offer is answered here without elicitation.
func (m *metaTools) routeProxiedCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}

		call, ok := request.(*mcp.CallToolRequest)
		if !ok || call.Params == nil {
			return next(ctx, method, request)
		}

		if _, _, proxied := splitProxiedName(call.Params.Name); !proxied {
			return next(ctx, method, request)
		}

		// A copy that has never synced, such as one just adopted from
		// another replica, gets its tools from the rows first, so a call
		// of an offered tool takes the go-sdk's tool path with its
		// elicitation.
		if err := m.syncUnsynced(ctx, call); err != nil {
			return nil, err
		}

		if m.offers(call, call.Params.Name) {
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

// syncUnsynced brings the proxied tools of the call's MCP session up to
// date when they have never been synced on this copy.
func (m *metaTools) syncUnsynced(ctx context.Context, call *mcp.CallToolRequest) error {
	current, ok := requestContextOf(call)
	if !ok {
		return nil
	}

	offered, found := m.offeredTools(call.Session, current)
	if !found {
		return nil
	}

	offered.mu.Lock()
	synced := offered.synced
	offered.mu.Unlock()

	if synced {
		return nil
	}

	_, err := m.syncSession(ctx, call.Session, current)

	return err
}

// offers reports whether the MCP session currently offers the proxied tool
// name.
func (m *metaTools) offers(call *mcp.CallToolRequest, name string) bool {
	current, _ := requestContextOf(call)

	offered, found := m.offeredTools(call.Session, current)
	if !found {
		return false
	}

	offered.mu.Lock()
	defer offered.mu.Unlock()

	_, ok := offered.offered[name]

	return ok
}

// offeredTools returns the proxied tools of the request's binding: a
// stateless binding's from the request, an MCP session's by its ID.
func (m *metaTools) offeredTools(session *mcp.ServerSession, current requestContext) (*sessionTools, bool) {
	if current.stateless != nil {
		return current.stateless, true
	}

	if session == nil || m.sessionTools == nil {
		return nil, false
	}

	return m.sessionTools(session.ID())
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
