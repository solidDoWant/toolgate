package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Names of the meta tools every session has.
const (
	ToolListServers   = "toolgate_list_servers"
	ToolEnableServer  = "toolgate_enable_server"
	ToolDisableServer = "toolgate_disable_server"
	ToolServerStatus  = "toolgate_server_status"
)

// Statuses shown to clients (SPEC.md §7).
const (
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusReady    = "ready"
	StatusFailed   = "failed"
	StatusBlocked  = "blocked"
)

// ServerEntry is one server in the toolgate_list_servers result.
type ServerEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope"`
	// Enabled says whether the server is enabled in this session.
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
	// Reason says why the server is blocked or failed.
	Reason string `json:"reason,omitempty"`
}

// ListServersResult is the toolgate_list_servers result.
type ListServersResult struct {
	Servers []ServerEntry `json:"servers"`
}

// ServerStatusResult is the toolgate_server_status result.
type ServerStatusResult struct {
	ServerEntry

	// ReadyAt is when the instance became ready.
	ReadyAt *time.Time `json:"readyAt,omitempty"`
	// Outdated says the running instance was started from an older version
	// of the template.
	Outdated bool `json:"outdated,omitempty"`
}

// ServerNameInput is the input of the tools that act on one server.
type ServerNameInput struct {
	Name string `json:"name" jsonschema:"the server name, as shown by toolgate_list_servers"`
}

type listServersInput struct{}

// metaTools implements the toolgate_* tools.
type metaTools struct {
	database    *pgxpool.Pool
	catalog     *catalog.Catalog
	policy      *policy.Engine
	blocks      blockCache
	auditOutput io.Writer
	// payloads stores call arguments and results; nil means off.
	payloads  *audit.Payloads
	subscribe func() (<-chan store.Event, func())
	upstreams *upstreams
	// sessionTools returns the proxied tool set of an MCP session.
	sessionTools func(mcpSessionID string) (*sessionTools, bool)
	// instanceLimitPerUser and instanceLimit bound the live instances of one
	// user and of the gateway.
	instanceLimitPerUser int
	instanceLimit        int
	sharedNamespace      string
	// approvalURL returns the web UI link of an approval; nil means the
	// gateway has no web UI.
	approvalURL     func(id string) string
	approvalTimeout time.Duration
}

func (m *metaTools) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolListServers,
		Description: "List the MCP servers you can enable, with their scope, status and whether they are enabled in this session.",
	}, m.listServers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolEnableServer,
		Description: "Start a server from the catalog, or reuse its running instance, and add its tools to this session.",
	}, m.enableServer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolDisableServer,
		Description: "Remove a server's tools from this session.",
	}, m.disableServer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolServerStatus,
		Description: "Show a server's status, the reason for its last failure, and when its instance became ready.",
	}, m.serverStatus)
}

func (m *metaTools) listServers(ctx context.Context, request *mcp.CallToolRequest, _ listServersInput) (*mcp.CallToolResult, ListServersResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, ListServersResult{}, errNoIdentity
	}

	enabled, err := m.enabledSet(ctx, current.session.ID)
	if err != nil {
		return nil, ListServersResult{}, err
	}

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, ListServersResult{}, err
	}

	result := ListServersResult{Servers: []ServerEntry{}}

	for _, template := range m.catalog.List() {
		entry, visible, err := m.entry(ctx, current, template, enabled, state)
		if err != nil {
			return nil, ListServersResult{}, err
		}

		if visible {
			result.Servers = append(result.Servers, entry)
		}
	}

	return nil, result, nil
}

func (m *metaTools) serverStatus(ctx context.Context, request *mcp.CallToolRequest, input ServerNameInput) (*mcp.CallToolResult, ServerStatusResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, ServerStatusResult{}, errNoIdentity
	}

	notFound := fmt.Errorf("Server `%s` isn't in your catalog. Call `%s` to see the servers you can use.", input.Name, ToolListServers) //nolint:staticcheck // A sentence for the model.

	template, err := m.catalog.Offered(input.Name)
	if err != nil {
		return nil, ServerStatusResult{}, notFound
	}

	enabled, err := m.enabledSet(ctx, current.session.ID)
	if err != nil {
		return nil, ServerStatusResult{}, err
	}

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, ServerStatusResult{}, err
	}

	entry, visible, err := m.entry(ctx, current, template, enabled, state)
	if err != nil {
		return nil, ServerStatusResult{}, err
	}

	if !visible {
		return nil, ServerStatusResult{}, notFound
	}

	result := ServerStatusResult{ServerEntry: entry}

	instance, found, err := m.newestInstance(ctx, current.identity, template)
	if err != nil {
		return nil, ServerStatusResult{}, err
	}

	if found && entry.Status == StatusReady {
		if instance.ReadyAt.Valid {
			readyAt := instance.ReadyAt.Time
			result.ReadyAt = &readyAt
		}

		result.Outdated = instance.Generation != template.Generation || instance.TemplateUid != string(template.UID)
	}

	return nil, result, nil
}

var errNoIdentity = errors.New("Sign in again: run `toolgate login`.") //nolint:staticcheck // A sentence for the model.

// entry describes template for the request's identity. visible is false when
// the identity has no catalog access to it; any other refusal shows the
// server as blocked with the reason.
func (m *metaTools) entry(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, enabled map[types.NamespacedName]bool, state policy.State) (entry ServerEntry, visible bool, err error) {
	decision := m.policy.Decide(policy.Request{Identity: current.identity, Template: template, State: state})
	if !decision.Allowed && decision.Step == policy.StepAccess {
		return ServerEntry{}, false, nil
	}

	entry = ServerEntry{
		Name:        template.Name,
		Description: template.Spec.Description,
		Scope:       template.Spec.Scope,
		Enabled:     enabled[types.NamespacedName{Namespace: template.Namespace, Name: template.Name}],
	}

	if !decision.Allowed {
		entry.Status = StatusBlocked
		entry.Reason = decision.Reason

		return entry, true, nil
	}

	instance, found, err := m.newestInstance(ctx, current.identity, template)
	if err != nil {
		return ServerEntry{}, false, err
	}

	entry.Status = StatusStopped
	if found {
		entry.Status, entry.Reason = shownStatus(instance)
	}

	return entry, true, nil
}

// shownStatus maps an instance row to the status shown to clients
// (docs/design/phase-1.md §2, "Status shown to clients").
func shownStatus(instance db.Instance) (status, reason string) {
	if instance.FailedAt.Valid {
		return StatusFailed, instance.Reason
	}

	switch instance.Status {
	case "pending", "starting":
		return StatusStarting, ""
	case "ready":
		return StatusReady, ""
	default:
		return StatusStopped, ""
	}
}

func (m *metaTools) newestInstance(ctx context.Context, identity auth.Identity, template *v1alpha1.MCPServerTemplate) (db.Instance, bool, error) {
	instance, err := db.New(m.database).GetNewestInstance(ctx, db.GetNewestInstanceParams{
		TemplateNs:   template.Namespace,
		TemplateName: template.Name,
		ScopeKey:     scopeKey(template.Spec.Scope, identity),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, false, nil
	}

	if err != nil {
		return db.Instance{}, false, fmt.Errorf("read instance: %w", err)
	}

	return instance, true, nil
}

func (m *metaTools) enabledSet(ctx context.Context, sessionPK int64) (map[types.NamespacedName]bool, error) {
	rows, err := db.New(m.database).ListSessionServers(ctx, sessionPK)
	if err != nil {
		return nil, fmt.Errorf("read enabled servers: %w", err)
	}

	enabled := make(map[types.NamespacedName]bool, len(rows))
	for _, row := range rows {
		enabled[types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName}] = true
	}

	return enabled, nil
}

// scopeKey is the key that decides which sessions share an instance
// (docs/design/phase-1.md §2).
func scopeKey(scope string, identity auth.Identity) string {
	switch scope {
	case "per-session":
		return "session:" + identity.Principal + ":" + identity.Session
	case "per-user":
		return "user:" + identity.Principal
	default:
		return "shared"
	}
}
