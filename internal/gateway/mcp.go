package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/version"
)

// MCPPath is where the gateway serves MCP (Streamable HTTP).
const MCPPath = "/mcp"

// maxRequestBodyBytes bounds an MCP request body (docs/design/phase-1.md §7).
const maxRequestBodyBytes = 4 << 20

const mcpSessionHeader = "Mcp-Session-Id"

// MCPOptions are the dependencies of the MCP endpoint.
type MCPOptions struct {
	// Database holds the sessions, session_servers and instances rows.
	Database *pgxpool.Pool
	// Catalog is the set of server templates.
	Catalog *catalog.Catalog
	// Policy decides what each identity may list and use.
	Policy *policy.Engine
	// AuditOutput receives the audit JSON lines. Nil means io.Discard.
	AuditOutput io.Writer
	// Logger logs transport errors. Nil means no logging.
	Logger *slog.Logger
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

// requestContext is what a tool handler knows about the request that called
// it. It travels in the go-sdk's TokenInfo, so every request carries its own
// identity and policy is decided from it.
type requestContext struct {
	identity auth.Identity
	session  db.Session
}

const requestContextKey = "toolgate"

// mcpBinding is what an MCP session is bound to at initialize. Every later
// request on it must authenticate to the same values.
type mcpBinding struct {
	principal string
	session   string
	workspace string
}

// mcpEndpoint serves MCP. It must run behind auth.Middleware with the
// session header required.
type mcpEndpoint struct {
	server   *mcp.Server
	handler  http.Handler
	sessions *sessionStore
	logger   *slog.Logger

	mu       sync.Mutex
	bindings map[string]mcpBinding
}

// NewMCPHandler returns the MCP endpoint. Wrap it in auth.Middleware with
// requireSession set; a request without an identity gets 401.
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
		sessions: &sessionStore{pool: options.Database, auditOutput: auditOutput, now: now},
		logger:   logger,
		bindings: make(map[string]mcpBinding),
	}

	endpoint.server = mcp.NewServer(&mcp.Implementation{Name: "toolgate", Version: version.Get()}, nil)

	tools := &metaTools{
		database: options.Database,
		catalog:  options.Catalog,
		policy:   options.Policy,
	}
	tools.register(endpoint.server)

	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return endpoint.server },
		&mcp.StreamableHTTPOptions{Logger: logger})

	// The go-sdk takes the request's identity from its TokenInfo. The token
	// was already verified by auth.Middleware.
	endpoint.handler = mcpauth.RequireBearerToken(endpoint.tokenInfo,
		&mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(streamable)

	return endpoint
}

func (e *mcpEndpoint) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok || identity.Session == "" {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)

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

	binding := mcpBinding{principal: identity.Principal, session: identity.Session, workspace: identity.Workspace}

	mcpSessionID := request.Header.Get(mcpSessionHeader)
	if mcpSessionID != "" {
		// A session this replica doesn't know, or one bound to another
		// identity, gets 404 so the client initializes again.
		if bound, found := e.binding(mcpSessionID); !found || bound != binding {
			http.Error(writer, "session not found", http.StatusNotFound)

			return
		}

		if request.Method == http.MethodDelete {
			defer e.unbind(mcpSessionID)
		}
	} else {
		writer = &bindingWriter{ResponseWriter: writer, onSessionID: func(id string) { e.bind(id, binding) }}
	}

	ctx := context.WithValue(request.Context(), requestContextCtxKey{}, requestContext{identity: identity, session: session})
	e.handler.ServeHTTP(writer, request.WithContext(ctx))
}

type requestContextCtxKey struct{}

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

func (e *mcpEndpoint) binding(id string) (mcpBinding, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	bound, found := e.bindings[id]

	return bound, found
}

// bind records a new MCP session's binding and forgets the bindings of
// sessions the go-sdk has closed.
func (e *mcpEndpoint) bind(id string, binding mcpBinding) {
	live := make(map[string]bool)
	for session := range e.server.Sessions() {
		live[session.ID()] = true
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for existing := range e.bindings {
		if !live[existing] {
			delete(e.bindings, existing)
		}
	}

	e.bindings[id] = binding
}

func (e *mcpEndpoint) unbind(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.bindings, id)
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
