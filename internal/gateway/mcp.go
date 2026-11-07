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

// maxRequestBodyBytes bounds an MCP request body.
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
	// Secrets returns a principal's secrets, which fill the headers of a
	// remote server's forwarding session. Nil means calls of remote servers
	// fail to connect.
	Secrets func(principal string) runtime.SecretSource
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
	// users. Set it only on a gateway with the
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
	// ToolSearchSessionStarted is called after a request starts or resumes
	// a session with tool search on. Nil does nothing.
	ToolSearchSessionStarted func(ctx context.Context)
}

// defaultSessionIdleTimeout is the SessionIdleTimeout when MCPOptions sets
// none. It matches the configuration's default.
const defaultSessionIdleTimeout = 2 * time.Hour

// defaultMCPSessionIdleTimeout is the MCPSessionIdleTimeout when MCPOptions
// sets none. A client of a released MCP session gets 404 and initializes
// again.
const defaultMCPSessionIdleTimeout = 30 * time.Minute

// The instance limits when MCPOptions sets none.
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
	// transport and serverSession are the go-sdk's halves of an MCP
	// session's copy. They are nil on a stateless binding.
	transport     *mcp.StreamableServerTransport
	serverSession *mcp.ServerSession

	// The fields below are guarded by mcpEndpoint.mu. active counts the
	// requests and streams in progress on the session; idle releases it
	// once active has been zero since lastActive for the idle timeout.
	active     int
	lastActive time.Time
	idle       *time.Timer
	// stored says the initialize of a new session committed its row.
	stored bool
	// lastTouch is when this replica last moved the row's last_seen_at.
	lastTouch time.Time
	// closing says the session ended: the copy is forgotten and closes
	// once its last request ends.
	closing bool
}

// mcpEndpoint serves MCP. It must run behind auth.Middleware in its MCP
// mode. Each MCP session gets its own mcp.Server, so a
// change to one session's proxied tools sends list_changed to it alone.
type mcpEndpoint struct {
	// newServer makes the server of an MCP session's copy, or of a
	// stateless binding.
	newServer func(stateless bool) *mcp.Server
	callTool  mcp.ToolHandler
	// toolSearch is the setting that decides the mode of a request without
	// the X-Toolgate-Tool-Search header.
	toolSearch bool
	// statefulHandler serves every revision before 2026-07-28 on the copy
	// ServeHTTP chose.
	statefulHandler http.Handler
	// discoverHandler serves server/discover on every revision.
	discoverHandler http.Handler
	// statelessHandler serves 2026-07-28 and later.
	statelessHandler http.Handler
	// discoverServer answers server/discover. It has no tools and makes no
	// MCP session.
	discoverServer *mcp.Server
	sessions       *sessionStore
	logger         *slog.Logger
	database       *pgxpool.Pool
	// endMCPSession is the end transaction of an MCP session that a client
	// deleted.
	endMCPSession func(ctx context.Context, id string) error

	// waiters counts the calls waiting for an approval.
	waiters *approvalWaiters

	idleTimeout time.Duration

	mu sync.Mutex
	// bindings holds this replica's copies of MCP sessions by
	// Mcp-Session-Id.
	bindings map[string]*mcpSession
	// adopting holds the copies being built from their rows.
	adopting map[string]*adoption
	// streams holds the open GET streams, for Shutdown and closeCopy.
	streams    map[uint64]openStream
	nextStream uint64
	// stateless holds this replica's stateless bindings, each with its own
	// mcp.Server, so list_changed reaches the binding's listen streams.
	stateless map[mcpBinding]*mcpSession
}

// MCPHandler is the MCP endpoint.
type MCPHandler interface {
	http.Handler
	// Shutdown ends the GET streams this replica serves, so their clients
	// reconnect to another replica. Call it when the gateway shuts down.
	Shutdown()
}

// NewMCPHandler returns the MCP endpoint. Wrap it in auth.Middleware with
// mcp set; a request without an identity gets 401.
func NewMCPHandler(options MCPOptions) MCPHandler {
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
		sessions: &sessionStore{
			pool: options.Database, auditOutput: auditOutput, now: now,
			toolSearchStarted: options.ToolSearchSessionStarted,
		},
		logger:     logger,
		toolSearch: options.ToolSearch,
		database:   options.Database,
		endMCPSession: func(ctx context.Context, id string) error {
			_, err := runtime.EndMCPSession(ctx, options.Database, auditOutput, id)

			return err
		},
		bindings: make(map[string]*mcpSession),
		adopting: make(map[string]*adoption),
		streams:  make(map[uint64]openStream),

		stateless: make(map[mcpBinding]*mcpSession),

		idleTimeout: options.MCPSessionIdleTimeout,
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
		upstreams:    newUpstreams(upstreamEndpoint, upstreamlimit.NewClient(upstreamHTTPClient, upstreamResponseBudget), options.Secrets),
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
	go endpoint.touchOpenSessions()

	implementation := &mcp.Implementation{Name: "toolgate", Version: version.Get()}

	endpoint.newServer = func(stateless bool) *mcp.Server {
		serverOptions := &mcp.ServerOptions{SetCacheable: privateToolLists}
		if stateless {
			serverOptions.SupportedProtocolVersions = []string{mcprequest.StatelessRevision}
		}

		server := mcp.NewServer(implementation, serverOptions)
		tools.register(server)
		server.AddReceivingMiddleware(tools.syncOnList, tools.routeProxiedCalls)

		// The row is written outermost, after the go-sdk made the
		// initialize result, and before the result goes out.
		if !stateless {
			server.AddReceivingMiddleware(endpoint.storeSessions)
		}

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

	endpoint.discoverHandler = requireToken(mcp.NewStreamableHTTPHandler(endpoint.serverFor,
		&mcp.StreamableHTTPOptions{Logger: logger}))
	endpoint.statefulHandler = requireToken(http.HandlerFunc(endpoint.serveCopy))
	endpoint.statelessHandler = requireToken(mcp.NewStreamableHTTPHandler(endpoint.statelessServerFor,
		&mcp.StreamableHTTPOptions{Logger: logger, Stateless: true}))

	return endpoint
}

// statelessNoSessionMessage answers a 2026-07-28 request without
// X-Toolgate-Session.
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
		e.discoverHandler.ServeHTTP(writer, request.WithContext(ctx))

		return
	}

	stateless := mcprequest.Stateless(request)
	mcpSessionID := request.Header.Get(mcpSessionHeader)

	if !stateless && !checkStreamableRequest(writer, request) {
		return
	}

	// A client without the session header gets a session tied to its MCP
	// session: mcp.<Mcp-Session-Id>. The binding check below refuses an ID
	// that wasn't made for this identity, so a client can't pick such a
	// session.
	connector := identity.Session == ""
	if connector && stateless {
		writeInvalidRequest(writer, body, statelessNoSessionMessage)

		return
	}

	var newSessionID string

	if !stateless && mcpSessionID == "" {
		if connector && mcprequest.MethodOf(request, body) != initializeMethod {
			http.Error(writer, "Send initialize first to start an MCP session.", http.StatusBadRequest)

			return
		}

		newSessionID, err = newMCPSessionID()
		if err != nil {
			e.logger.ErrorContext(request.Context(), "session ID generation failed", "error", err)
			http.Error(writer, "The gateway can't start a session right now. Try again later.", http.StatusServiceUnavailable)

			return
		}
	}

	if connector {
		identity.Session = auth.MCPSessionPrefix + mcpSessionID + newSessionID
	}

	binding := mcpBinding{
		principal: identity.Principal, session: identity.Session,
		workspace: identity.Workspace, attestation: identity.Attestation,
	}

	// A request on an MCP session that doesn't exist, ended, or is bound to
	// another identity, workspace or attestation gets 404 before the
	// session lookup's 400, so the client initializes again and then
	// learns the workspace changed. A session this replica doesn't hold is
	// adopted from its row.
	var held *mcpSession

	if !stateless && mcpSessionID != "" {
		held, err = e.acquireOrAdopt(request.Context(), mcpSessionID, binding)
		if errors.Is(err, errMCPSessionNotFound) {
			writeSessionNotFound(writer)

			return
		}

		if err != nil {
			e.logger.ErrorContext(request.Context(), "MCP session lookup failed", "error", err)
			http.Error(writer, "The gateway can't reach its database. Try again later.", http.StatusServiceUnavailable)

			return
		}

		// The session counts as in use until the request ends, which
		// for a GET is when its stream closes.
		defer e.release(mcpSessionID, held)

		if err := e.touch(request.Context(), mcpSessionID, held); err != nil {
			writeSessionNotFound(writer)

			return
		}

		if request.Method == http.MethodDelete {
			e.deleteSession(request.Context(), writer, mcpSessionID)

			return
		}
	}

	searchMode := searchModeOf(request, e.toolSearch)

	session, err := e.sessions.resolve(request.Context(), identity, searchMode)
	if errors.Is(err, errWorkspaceChanged) {
		http.Error(writer, "This session started in a different workspace. Start a new Claude Code session to use another workspace.", http.StatusBadRequest)

		return
	}

	// The MCP session ended while this request was on its way, so the
	// client initializes again.
	if errors.Is(err, errSessionEnded) {
		writeSessionNotFound(writer)

		return
	}

	if err != nil {
		e.logger.ErrorContext(request.Context(), "session lookup failed", "error", err)
		http.Error(writer, "The gateway can't reach its database. Try again later.", http.StatusServiceUnavailable)

		return
	}

	current := requestContext{identity: identity, session: session, searchMode: searchMode}

	if stateless {
		e.serveStateless(writer, request, binding, current)

		return
	}

	if held == nil {
		// A new MCP session gets its own copy. It is in use until the
		// initialize request ends, and closed then unless its row was
		// stored.
		held, err = e.startSession(request.Context(), newSessionID, binding, current)
		if err != nil {
			e.logger.ErrorContext(request.Context(), "starting an MCP session failed", "error", err)
			http.Error(writer, "The gateway can't start a session right now. Try again later.", http.StatusServiceUnavailable)

			return
		}

		defer func() {
			if !e.isStored(held) {
				e.closeCopy(newSessionID)
			}

			e.release(newSessionID, held)
		}()
	} else {
		held.tools.setCurrent(current)
	}

	ctx := context.WithValue(request.Context(), requestContextCtxKey{}, current)
	ctx = context.WithValue(ctx, mcpCopyCtxKey{}, held)

	// A GET stream ends when the request's context does, so Shutdown can
	// end it.
	if request.Method == http.MethodGet {
		var cancel context.CancelFunc

		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer e.trackStream(held, cancel)()
	}

	e.statefulHandler.ServeHTTP(writer, request.WithContext(ctx))
}

// serveCopy serves a request on the transport of the copy ServeHTTP chose.
func (e *mcpEndpoint) serveCopy(writer http.ResponseWriter, request *http.Request) {
	held, _ := request.Context().Value(mcpCopyCtxKey{}).(*mcpSession)
	held.transport.ServeHTTP(writer, request)
}

// isStored reports whether the initialize of a new session stored its row.
func (e *mcpEndpoint) isStored(held *mcpSession) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return held.stored
}

type mcpCopyCtxKey struct{}

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
		tools := newSessionTools(e.newServer(true), e.callTool, current)
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
// dual-era client falls back to initialize.
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

// serverFor returns the discover server ServeHTTP chose.
func (e *mcpEndpoint) serverFor(request *http.Request) *mcp.Server {
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

// release ends a request that acquireOrAdopt or startSession started. When it was the session's
// last one, the idle timer starts.
func (e *mcpEndpoint) release(id string, session *mcpSession) {
	e.mu.Lock()
	defer e.mu.Unlock()

	session.active--
	if session.active == 0 && session.closing {
		// An ended session's copy closes after its last request, so a
		// call in progress still gets its answer.
		go func() { _ = session.serverSession.Close() }()

		return
	}

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

// expire releases this replica's copy of an MCP session that has been idle
// for the idle timeout: it forgets the copy and closes it, which ends its
// goroutines. The session stays live: a later request on any replica adopts
// it again.
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

	_ = session.serverSession.Close()
}

// endSessionTimeout bounds the end transaction of an MCP session.
const endSessionTimeout = 30 * time.Second

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

// requestContextOf returns the identity and session of the request that
// called a tool.
func requestContextOf(request *mcp.CallToolRequest) (requestContext, bool) {
	if request.Extra == nil || request.Extra.TokenInfo == nil {
		return requestContext{}, false
	}

	current, ok := request.Extra.TokenInfo.Extra[requestContextKey].(requestContext)

	return current, ok
}
