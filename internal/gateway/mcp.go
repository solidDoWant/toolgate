package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/mcprequest"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
	"github.com/solidDoWant/toolgate/internal/version"
)

// MCPPath is where the gateway serves MCP (Streamable HTTP).
const MCPPath = "/mcp"

// maxRequestBodyBytes bounds an MCP request body (docs/design/phase-1.md §7).
const maxRequestBodyBytes = 4 << 20

const mcpSessionHeader = "Mcp-Session-Id"

// MCPOptions are the dependencies of the MCP endpoint.
type MCPOptions struct {
	// Metrics counts tool calls and policy refusals. Nil means none.
	Metrics *runtime.Metrics
	// Database holds the sessions, session_servers and instances rows.
	Database *pgxpool.Pool
	// Catalog is the set of server templates.
	Catalog *catalog.Catalog
	// Policy decides what each identity may list and use.
	Policy *policy.Engine
	// AuditOutput receives the audit JSON lines. Nil means io.Discard.
	AuditOutput io.Writer
	// AuditPayloads stores the arguments and results of tool calls. Nil
	// means audit payloads are off.
	AuditPayloads *audit.Payloads
	// Logger logs transport errors. Nil means no logging.
	Logger *slog.Logger
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// Subscribe returns the PostgreSQL notifications and a function that
	// ends the subscription (store.Listener.Subscribe). Waiting enables
	// wake on them. Nil means no notifications: waits rely on the progress
	// tick alone.
	Subscribe func() (<-chan store.Event, func())
	// InstanceLimitPerUser is the most live instances one user may have.
	// Zero means 20.
	InstanceLimitPerUser int
	// InstanceLimit is the most live instances the gateway runs. Zero means
	// 200.
	InstanceLimit int
	// SharedNamespace is the namespace that instances of `namespace: shared`
	// templates are applied in. The catalog refuses such templates when it
	// is empty.
	SharedNamespace string
	// UpstreamEndpoint returns the MCP URL of an instance. Nil means the
	// URL of the template's endpoint Service in the instance's namespace.
	UpstreamEndpoint UpstreamEndpoint
	// UpstreamHTTPClient carries forwarded calls. Nil means
	// http.DefaultClient.
	UpstreamHTTPClient *http.Client
	// MCPSessionIdleTimeout is how long an MCP session may go without a
	// request in progress or an open stream before this replica releases
	// it. Zero means 30 minutes.
	MCPSessionIdleTimeout time.Duration
	// ApprovalURL returns the web UI link of an approval. Nil means the
	// gateway has no web UI, so a tool that needs approval can't be called.
	ApprovalURL func(id string) string
	// ApprovalTimeout is how long an approval waits for a person and stays
	// usable once approved. Zero means DefaultApprovalTimeout.
	ApprovalTimeout time.Duration
	// WorkspaceScoping turns on the workspace-scope step for signed-in
	// users (docs/design/phase-3.md §10). Set it only on a gateway with the
	// web UI: without it the step is off.
	WorkspaceScoping bool
	// Workspaces serves the built-in server's tools with the same methods
	// as the workspaces API. Nil means a service over Database and Catalog.
	Workspaces *Workspaces
	// SessionIdleTimeout is the toolgate session idle timeout: the built-in
	// server lists the open sessions seen within it, as GET
	// /api/v1/sessions does. Zero means defaultSessionIdleTimeout.
	SessionIdleTimeout time.Duration
	// ToolSearch is the gateway's default for tool search mode, used by a
	// request that sends no X-Toolgate-Tool-Search header.
	ToolSearch bool
	// Embedder embeds search queries for tool search; nil means words only.
	Embedder *embeddings.Client
}

// defaultSessionIdleTimeout is the SessionIdleTimeout when MCPOptions sets
// none. It matches the configuration's default.
const defaultSessionIdleTimeout = 2 * time.Hour

// defaultMCPSessionIdleTimeout is the MCPSessionIdleTimeout when MCPOptions
// sets none. A client of a released MCP session gets 404 and initializes
// again (docs/design/phase-1.md §3).
const defaultMCPSessionIdleTimeout = 30 * time.Minute

// The instance limits when MCPOptions sets none (docs/design/phase-1.md §7).
const (
	defaultInstanceLimitPerUser = 20
	defaultInstanceLimit        = 200
)

func valueOr[T int | time.Duration](value, fallback T) T {
	if value == 0 {
		return fallback
	}

	return value
}

// requestContext is what a tool handler knows about the request that called
// it. It travels in the go-sdk's TokenInfo, so every request carries its own
// identity and policy is decided from it.
type requestContext struct {
	identity auth.Identity
	session  db.Session
	// stateless is the proxied tools of the stateless binding a 2026-07-28
	// request is served on. It is nil on an MCP session, whose tools are
	// found by its ID.
	stateless *sessionTools
	// searchMode says the request asked for tool search mode, by header or
	// by the gateway's setting. It only changes what a tool list holds.
	searchMode bool
}

const requestContextKey = "toolgate"

// mcpBinding is what an MCP session is bound to at initialize. Every later
// request on it must authenticate to the same values: the identity's final
// workspace and attestation, not the headers as sent.
type mcpBinding struct {
	principal   string
	session     string
	workspace   string
	attestation string
}

// mcpSession is one MCP session this replica holds: its binding and its own
// mcp.Server.
type mcpSession struct {
	binding mcpBinding
	tools   *sessionTools
	// connectorSession is the sessions row of a session tied to this MCP
	// session (auth.MCPSessionPrefix), which ends with it. Zero otherwise.
	connectorSession int64

	// The fields below are guarded by mcpEndpoint.mu. active counts the
	// requests and streams in progress on the session; idle releases it
	// once active has been zero since lastActive for the idle timeout.
	active     int
	lastActive time.Time
	idle       *time.Timer
}

// mcpEndpoint serves MCP. It must run behind auth.Middleware in its MCP
// mode. Each MCP session gets its own mcp.Server, so a
// change to one session's proxied tools sends list_changed to it alone.
type mcpEndpoint struct {
	// newServer makes an MCP session's server. A non-empty sessionID is the
	// Mcp-Session-Id the go-sdk assigns to the session it initializes.
	newServer func(stateless bool, sessionID string) *mcp.Server
	callTool  mcp.ToolHandler
	// toolSearch is the setting that decides the mode of a request without
	// the X-Toolgate-Tool-Search header.
	toolSearch bool
	// handler is the stateful handler. It serves every revision before
	// 2026-07-28, and server/discover on every revision.
	handler http.Handler
	// statelessHandler serves 2026-07-28 and later.
	statelessHandler http.Handler
	// discoverServer answers server/discover. It has no tools and makes no
	// MCP session.
	discoverServer *mcp.Server
	sessions       *sessionStore
	logger         *slog.Logger
	// endSession ends a session tied to an MCP session that closed.
	endSession func(ctx context.Context, id int64, reason string) error

	// waiters counts the calls waiting for an approval.
	waiters *approvalWaiters

	idleTimeout time.Duration

	mu       sync.Mutex
	bindings map[string]*mcpSession
	// stateless holds this replica's stateless bindings, each with its own
	// mcp.Server, so list_changed reaches the binding's listen streams.
	stateless map[mcpBinding]*mcpSession
}

// NewMCPHandler returns the MCP endpoint. Wrap it in auth.Middleware with
// mcp set; a request without an identity gets 401.
func NewMCPHandler(options MCPOptions) http.Handler {
	auditOutput := options.AuditOutput
	if auditOutput == nil {
		auditOutput = io.Discard
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	endpoint := &mcpEndpoint{
		sessions:   &sessionStore{pool: options.Database, auditOutput: auditOutput, now: now},
		logger:     logger,
		toolSearch: options.ToolSearch,
		bindings:   make(map[string]*mcpSession),

		stateless: make(map[mcpBinding]*mcpSession),

		idleTimeout: options.MCPSessionIdleTimeout,
		endSession: func(ctx context.Context, id int64, reason string) error {
			return runtime.EndSession(ctx, options.Database, auditOutput, id, reason)
		},
	}

	if endpoint.idleTimeout <= 0 {
		endpoint.idleTimeout = testtime.Scale(defaultMCPSessionIdleTimeout)
	}

	subscribe := options.Subscribe
	if subscribe == nil {
		subscribe = func() (<-chan store.Event, func()) { return nil, func() {} }
	}

	upstreamEndpoint := options.UpstreamEndpoint
	if upstreamEndpoint == nil {
		upstreamEndpoint = runtime.EndpointURL
	}

	upstreamHTTPClient := options.UpstreamHTTPClient
	if upstreamHTTPClient == nil {
		upstreamHTTPClient = http.DefaultClient
	}

	tools := &metaTools{
		metrics:      options.Metrics,
		upstreams:    newUpstreams(upstreamEndpoint, upstreamlimit.NewClient(upstreamHTTPClient, upstreamResponseBudget)),
		database:     options.Database,
		catalog:      options.Catalog,
		policy:       options.Policy,
		auditOutput:  auditOutput,
		payloads:     options.AuditPayloads,
		subscribe:    subscribe,
		sessionTools: endpoint.sessionTools,

		instanceLimitPerUser: valueOr(options.InstanceLimitPerUser, defaultInstanceLimitPerUser),
		instanceLimit:        valueOr(options.InstanceLimit, defaultInstanceLimit),
		sharedNamespace:      options.SharedNamespace,
		approvalURL:          options.ApprovalURL,
		approvalTimeout:      valueOr(options.ApprovalTimeout, DefaultApprovalTimeout),
		workspaceScoping:     options.WorkspaceScoping,
		workspaces:           options.Workspaces,
		sessionIdleTimeout:   valueOr(options.SessionIdleTimeout, defaultSessionIdleTimeout),
		embedder:             options.Embedder,
	}

	if tools.workspaces == nil {
		tools.workspaces = NewWorkspaces(options.Database, options.Catalog, auditOutput, options.ApprovalTimeout)
	}

	endpoint.callTool = tools.callTool
	endpoint.waiters = &tools.waiters
	endpoint.sessions.restore = tools.restoreServers

	var (
		events  <-chan store.Event
		changed catalogChanges
	)

	if options.Subscribe != nil {
		events, _ = options.Subscribe()
	}

	if options.Policy != nil && options.Policy.Rules != nil {
		changed.rules, _ = options.Policy.Rules.Subscribe()
	}

	if options.Catalog != nil {
		changed.templates, _ = options.Catalog.Subscribe()
	}

	go endpoint.fanOut(tools, events, changed)

	implementation := &mcp.Implementation{Name: "toolgate", Version: version.Get()}

	endpoint.newServer = func(stateless bool, sessionID string) *mcp.Server {
		serverOptions := &mcp.ServerOptions{SetCacheable: privateToolLists}
		if stateless {
			serverOptions.SupportedProtocolVersions = []string{mcprequest.StatelessRevision}
		}

		// GetSessionID can't read the request, but a server is made for
		// one initialize, so the closure hands the go-sdk the ID chosen
		// for it.
		if sessionID != "" {
			serverOptions.GetSessionID = func() string { return sessionID }
		}

		server := mcp.NewServer(implementation, serverOptions)
		tools.register(server)
		server.AddReceivingMiddleware(tools.syncOnList, tools.routeProxiedCalls)

		return server
	}

	// An empty session ID makes the go-sdk serve a discover on an
	// ephemeral session it never binds.
	endpoint.discoverServer = mcp.NewServer(implementation, &mcp.ServerOptions{
		SetCacheable: privateToolLists,
		GetSessionID: func() string { return "" },
	})

	// The go-sdk takes the request's identity from its TokenInfo. The token
	// was already verified by auth.Middleware.
	requireToken := mcpauth.RequireBearerToken(endpoint.tokenInfo,
		&mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})

	endpoint.handler = requireToken(mcp.NewStreamableHTTPHandler(endpoint.serverFor,
		&mcp.StreamableHTTPOptions{Logger: logger}))
	endpoint.statelessHandler = requireToken(mcp.NewStreamableHTTPHandler(endpoint.statelessServerFor,
		&mcp.StreamableHTTPOptions{Logger: logger, Stateless: true}))

	return endpoint
}

// statelessNoSessionMessage answers a 2026-07-28 request without
// X-Toolgate-Session (docs/design/phase-4.md §8).
const statelessNoSessionMessage = "This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25."

// privateToolLists marks every tools/list result private and uncached, so
// no cache shares one session's tools with another or hides a change. On
// revisions before 2026-07-28 the fields are not on the wire.
func privateToolLists(_ context.Context, request mcp.Request, cacheable *mcp.Cacheable) {
	if _, ok := request.(*mcp.ListToolsRequest); ok {
		cacheable.CacheScope = "private"
		cacheable.TTLMs = 0
	}
}

func (e *mcpEndpoint) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	// The body is read here, bounded, so an oversized request gets a clear
	// error instead of a transport failure.
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, "The request is larger than 4 MiB, the limit. Send smaller tool arguments.", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(writer, "The request body can't be read.", http.StatusBadRequest)

		return
	}

	request.Body = io.NopCloser(bytes.NewReader(body))

	opsmetrics.MCPRequest(request.Header.Get(mcprequest.ProtocolVersionHeader), mcprequest.MethodOf(request, body))

	// A server/discover needs no session on any revision, and makes none:
	// the stateful handler answers it from the shared discover server.
	if mcprequest.MethodOf(request, body) == mcprequest.DiscoverMethod {
		ctx := context.WithValue(request.Context(), requestContextCtxKey{}, requestContext{identity: identity})
		ctx = context.WithValue(ctx, newServerCtxKey{}, e.discoverServer)

		request.Header.Del(mcpSessionHeader)
		e.handler.ServeHTTP(writer, request.WithContext(ctx))

		return
	}

	stateless := mcprequest.Stateless(request)
	mcpSessionID := request.Header.Get(mcpSessionHeader)

	// A client without the session header gets a session tied to its MCP
	// session: a new ID at initialize, and mcp.<Mcp-Session-Id> after
	// (docs/design/phase-4.md §9). The binding check below refuses an ID
	// this replica didn't assign to this identity, so a client can't pick
	// such a session.
	var newSessionID string

	connector := identity.Session == ""
	if connector {
		if stateless {
			writeInvalidRequest(writer, body, statelessNoSessionMessage)

			return
		}

		if mcpSessionID == "" {
			if mcprequest.MethodOf(request, body) != initializeMethod {
				http.Error(writer, "Send initialize first to start an MCP session.", http.StatusBadRequest)

				return
			}

			newSessionID, err = newMCPSessionID()
			if err != nil {
				e.logger.ErrorContext(request.Context(), "session ID generation failed", "error", err)
				http.Error(writer, "The gateway can't start a session right now. Try again later.", http.StatusServiceUnavailable)

				return
			}

			identity.Session = auth.MCPSessionPrefix + newSessionID
		} else {
			identity.Session = auth.MCPSessionPrefix + mcpSessionID
		}
	}

	binding := mcpBinding{
		principal: identity.Principal, session: identity.Session,
		workspace: identity.Workspace, attestation: identity.Attestation,
	}

	// A request on an MCP session bound to another workspace or
	// attestation gets 404 before the session lookup's 400, so the client
	// initializes again and then learns the workspace changed.
	if !stateless && mcpSessionID != "" && !e.boundTo(mcpSessionID, binding) {
		http.Error(writer, "session not found", http.StatusNotFound)

		return
	}

	session, err := e.sessions.resolve(request.Context(), identity)
	if errors.Is(err, errWorkspaceChanged) {
		http.Error(writer, "This session started in a different workspace. Start a new Claude Code session to use another workspace.", http.StatusBadRequest)

		return
	}

	if err != nil {
		e.logger.ErrorContext(request.Context(), "session lookup failed", "error", err)
		http.Error(writer, "The gateway can't reach its database. Try again later.", http.StatusServiceUnavailable)

		return
	}

	current := requestContext{identity: identity, session: session, searchMode: searchModeOf(request, e.toolSearch)}

	if stateless {
		e.serveStateless(writer, request, binding, current)

		return
	}

	ctx := context.WithValue(request.Context(), requestContextCtxKey{}, current)

	if mcpSessionID != "" {
		// A session this replica doesn't know, or one bound to another
		// identity, gets 404 so the client initializes again.
		known, found := e.acquire(mcpSessionID, binding)
		if !found {
			http.Error(writer, "session not found", http.StatusNotFound)

			return
		}

		// The session counts as in use until the request ends, which
		// for a GET is when its stream closes.
		defer e.release(mcpSessionID, known)

		if request.Method == http.MethodDelete {
			defer e.unbind(mcpSessionID)

			if known.connectorSession != 0 {
				defer e.endConnectorSession(request.Context(), known.connectorSession, endReasonDelete)
			}
		}

		known.tools.setCurrent(current)
	}

	if mcpSessionID == "" {
		// A new MCP session gets its own server, bound once the go-sdk
		// assigns its ID. It is in use until the initialize request ends.
		var (
			created   *mcpSession
			createdID string
		)

		server := e.newServer(false, newSessionID)
		ctx = context.WithValue(ctx, newServerCtxKey{}, server)
		writer = &bindingWriter{ResponseWriter: writer, onSessionID: func(id string) {
			created = &mcpSession{binding: binding, tools: newSessionTools(server, e.callTool, current), active: 1}
			if connector {
				created.connectorSession = session.ID
			}

			createdID = id
			e.bind(id, created)
		}}

		defer func() {
			if created != nil {
				e.release(createdID, created)
			}
		}()
	}

	e.handler.ServeHTTP(writer, request.WithContext(ctx))
}

// serveStateless serves a 2026-07-28 request on its stateless binding's own
// mcp.Server. The binding is in use until the request ends, which for a
// subscriptions/listen is when its stream closes.
func (e *mcpEndpoint) serveStateless(writer http.ResponseWriter, request *http.Request, binding mcpBinding, current requestContext) {
	held, current := e.acquireStateless(binding, current)
	defer e.releaseStateless(binding, held)

	ctx := context.WithValue(request.Context(), requestContextCtxKey{}, current)
	ctx = context.WithValue(ctx, newServerCtxKey{}, held.tools.server)

	e.statelessHandler.ServeHTTP(writer, request.WithContext(ctx))
}

// statelessServerFor returns the server serveStateless chose.
func (e *mcpEndpoint) statelessServerFor(request *http.Request) *mcp.Server {
	server, _ := request.Context().Value(newServerCtxKey{}).(*mcp.Server)

	return server
}

// acquireStateless returns the stateless binding, made with its own server
// when this replica holds none, and marks a request on it as in progress.
// It records current, pointed at the binding's tools, as the binding's
// latest request and returns it.
func (e *mcpEndpoint) acquireStateless(binding mcpBinding, current requestContext) (*mcpSession, requestContext) {
	e.mu.Lock()
	defer e.mu.Unlock()

	held, found := e.stateless[binding]
	if !found {
		tools := newSessionTools(e.newServer(true, ""), e.callTool, current)
		held = &mcpSession{binding: binding, tools: tools}
		e.stateless[binding] = held
	}

	current.stateless = held.tools
	held.tools.setCurrent(current)
	held.active++

	return held, current
}

// releaseStateless ends a request acquireStateless started. When it was the
// binding's last one, the idle timer starts.
func (e *mcpEndpoint) releaseStateless(binding mcpBinding, held *mcpSession) {
	e.mu.Lock()
	defer e.mu.Unlock()

	held.active--
	if held.active > 0 || e.stateless[binding] != held {
		return
	}

	held.lastActive = time.Now()

	if held.idle == nil {
		held.idle = time.AfterFunc(e.idleTimeout, func() { e.expireStateless(binding, held) })

		return
	}

	held.idle.Reset(e.idleTimeout)
}

// expireStateless releases a stateless binding that has been idle for the
// idle timeout. Its requests ran on ephemeral go-sdk sessions, which are
// already closed. The next request makes a new binding.
func (e *mcpEndpoint) expireStateless(binding mcpBinding, held *mcpSession) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stateless[binding] != held || held.active > 0 || time.Since(held.lastActive) < e.idleTimeout {
		return
	}

	delete(e.stateless, binding)
}

// writeInvalidRequest answers with HTTP 400 and a plain JSON-RPC invalid
// request error, never the go-sdk's unsupported version error, so a
// dual-era client falls back to initialize (docs/design/phase-4.md §8).
func writeInvalidRequest(writer http.ResponseWriter, body []byte, message string) {
	response := &jsonrpc.Response{Error: &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: message}}

	if decoded, err := jsonrpc.DecodeMessage(body); err == nil {
		if call, ok := decoded.(*jsonrpc.Request); ok {
			response.ID = call.ID
		}
	}

	encoded, err := jsonrpc.EncodeMessage(response)
	if err != nil {
		http.Error(writer, message, http.StatusBadRequest)

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write(encoded)
}

type requestContextCtxKey struct{}

type newServerCtxKey struct{}

// serverFor returns the mcp.Server of the request's MCP session, or the new
// server ServeHTTP made for a request that starts one.
func (e *mcpEndpoint) serverFor(request *http.Request) *mcp.Server {
	if id := request.Header.Get(mcpSessionHeader); id != "" {
		if session, found := e.session(id); found {
			return session.tools.server
		}

		return nil
	}

	server, _ := request.Context().Value(newServerCtxKey{}).(*mcp.Server)

	return server
}

// tokenInfo hands the request's identity and session to the go-sdk. UserID
// makes the go-sdk refuse a request bound to another identity too.
func (e *mcpEndpoint) tokenInfo(ctx context.Context, _ string, _ *http.Request) (*mcpauth.TokenInfo, error) {
	current, ok := ctx.Value(requestContextCtxKey{}).(requestContext)
	if !ok {
		return nil, mcpauth.ErrInvalidToken
	}

	identity := current.identity

	return &mcpauth.TokenInfo{
		UserID: identity.Principal + "\x00" + identity.Session + "\x00" + identity.Workspace,
		Extra:  map[string]any{requestContextKey: current},
	}, nil
}

func (e *mcpEndpoint) session(id string) (*mcpSession, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	session, found := e.bindings[id]

	return session, found
}

func (e *mcpEndpoint) sessionTools(id string) (*sessionTools, bool) {
	session, found := e.session(id)
	if !found {
		return nil, false
	}

	return session.tools, true
}

// bind records a new MCP session and forgets the sessions the go-sdk has
// closed.
func (e *mcpEndpoint) bind(id string, session *mcpSession) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for existing, known := range e.bindings {
		if !hasSession(known.tools.server, existing) {
			e.forget(existing, known)
		}
	}

	e.bindings[id] = session
}

// boundTo reports whether this replica holds the MCP session id bound to
// binding.
func (e *mcpEndpoint) boundTo(id string, binding mcpBinding) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	session, found := e.bindings[id]

	return found && session.binding == binding
}

// acquire marks a request on the MCP session id as in progress. It returns
// false when this replica doesn't hold the session or it is bound to
// another identity.
func (e *mcpEndpoint) acquire(id string, binding mcpBinding) (*mcpSession, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	session, found := e.bindings[id]
	if !found || session.binding != binding {
		return nil, false
	}

	session.active++

	return session, true
}

// release ends a request that acquire started. When it was the session's
// last one, the idle timer starts.
func (e *mcpEndpoint) release(id string, session *mcpSession) {
	e.mu.Lock()
	defer e.mu.Unlock()

	session.active--
	if session.active > 0 || e.bindings[id] != session {
		return
	}

	session.lastActive = time.Now()

	if session.idle == nil {
		session.idle = time.AfterFunc(e.idleTimeout, func() { e.expire(id, session) })

		return
	}

	session.idle.Reset(e.idleTimeout)
}

// expire releases an MCP session that has been idle for the idle timeout:
// it forgets the binding and closes the go-sdk session, which ends its
// goroutines. A later request with its ID gets 404.
func (e *mcpEndpoint) expire(id string, session *mcpSession) {
	e.mu.Lock()

	// A request may have started, or ended and restarted the timer, since
	// the timer fired.
	if e.bindings[id] != session || session.active > 0 || time.Since(session.lastActive) < e.idleTimeout {
		e.mu.Unlock()

		return
	}

	e.forget(id, session)
	e.mu.Unlock()

	for open := range session.tools.server.Sessions() {
		if open.ID() == id {
			_ = open.Close()
		}
	}

	if session.connectorSession != 0 {
		e.endConnectorSession(context.Background(), session.connectorSession, endReasonIdle)
	}
}

// The session.end reasons of a session tied to an MCP session.
const (
	endReasonDelete = "mcp-session-delete"
	endReasonIdle   = "mcp-session-idle"
)

// endSessionTimeout bounds the end transaction of a session tied to an MCP
// session.
const endSessionTimeout = 30 * time.Second

// endConnectorSession ends the session tied to an MCP session that closed.
// It runs after the request or the idle timer, so the client's
// cancellation can't stop it. A failure is logged: the session then ends
// on the reconciler's idle pass, and nothing can resume it because its
// MCP session is gone.
func (e *mcpEndpoint) endConnectorSession(ctx context.Context, id int64, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endSessionTimeout)
	defer cancel()

	if err := e.endSession(ctx, id, reason); err != nil {
		e.logger.ErrorContext(ctx, "ending the session of a closed MCP session failed", "error", err)
	}
}

// initializeMethod is the JSON-RPC method that starts an MCP session.
const initializeMethod = "initialize"

// newMCPSessionID returns the Mcp-Session-Id of a new MCP session of a
// client without the session header: 32 lowercase hex characters from
// crypto/rand.
func newMCPSessionID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}

	return hex.EncodeToString(random), nil
}

// forget removes a binding and stops its idle timer. The caller holds e.mu.
func (e *mcpEndpoint) forget(id string, session *mcpSession) {
	if session.idle != nil {
		session.idle.Stop()
	}

	delete(e.bindings, id)
}

func hasSession(server *mcp.Server, id string) bool {
	for session := range server.Sessions() {
		if session.ID() == id {
			return true
		}
	}

	return false
}

func (e *mcpEndpoint) unbind(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if session, found := e.bindings[id]; found {
		e.forget(id, session)
	}
}

// bindingWriter reports the Mcp-Session-Id the go-sdk assigns at initialize,
// before the response reaches the client.
type bindingWriter struct {
	http.ResponseWriter

	onSessionID func(string)
	reported    bool
}

func (w *bindingWriter) WriteHeader(status int) {
	w.report()
	w.ResponseWriter.WriteHeader(status)
}

func (w *bindingWriter) Write(data []byte) (int, error) {
	w.report()

	return w.ResponseWriter.Write(data)
}

func (w *bindingWriter) Flush() {
	w.report()

	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *bindingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *bindingWriter) report() {
	if w.reported {
		return
	}

	w.reported = true

	if id := w.Header().Get(mcpSessionHeader); id != "" {
		w.onSessionID(id)
	}
}

// requestContextOf returns the identity and session of the request that
// called a tool.
func requestContextOf(request *mcp.CallToolRequest) (requestContext, bool) {
	if request.Extra == nil || request.Extra.TokenInfo == nil {
		return requestContext{}, false
	}

	current, ok := request.Extra.TokenInfo.Extra[requestContextKey].(requestContext)

	return current, ok
}
