package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Every replica serves every MCP session. Its state is an mcp_sessions row,
// and a replica keeps a copy in memory only while it serves the session. The
// go-sdk's StreamableHTTPHandler keeps its sessions in a private map that
// can't be filled from a row, so the endpoint holds the transports itself
// and builds a copy from the row when a request names a session it doesn't
// hold.

// touchInterval is how often a replica moves last_seen_at of a session it
// serves, or that has a GET stream open on it.
const touchInterval = time.Minute

// mcpDatabaseTimeout bounds one read or write of an mcp_sessions row.
const mcpDatabaseTimeout = 10 * time.Second

// errMCPSessionNotFound is the 404 of an MCP session: no row, an ended
// row, or a row bound to another identity all look the same to the client.
var errMCPSessionNotFound = errors.New("session not found")

// adoption is a copy being built from its row. Concurrent misses for one ID
// wait for it instead of building their own.
type adoption struct {
	done chan struct{}
}

// writeSessionNotFound answers a request for an MCP session that this
// replica can't serve. The body is the same for every reason.
func writeSessionNotFound(writer http.ResponseWriter) {
	http.Error(writer, errMCPSessionNotFound.Error(), http.StatusNotFound)
}

// checkStreamableRequest keeps the go-sdk handler's checks of a request
// before it reaches a transport. It reports false when it answered.
func checkStreamableRequest(writer http.ResponseWriter, request *http.Request) bool {
	switch request.Method {
	case http.MethodGet:
		if _, streamOK := streamableAccepts(request.Header.Values("Accept")); !streamOK {
			http.Error(writer, "Accept must contain 'text/event-stream' for GET requests", http.StatusBadRequest)

			return false
		}

		if request.Header.Get(mcpSessionHeader) == "" {
			http.Error(writer, "Bad Request: GET requires an Mcp-Session-Id header", http.StatusBadRequest)

			return false
		}
	case http.MethodPost:
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(writer, "Content-Type must be 'application/json'", http.StatusUnsupportedMediaType)

			return false
		}

		if jsonOK, streamOK := streamableAccepts(request.Header.Values("Accept")); !jsonOK || !streamOK {
			http.Error(writer, "Accept must contain both 'application/json' and 'text/event-stream'", http.StatusBadRequest)

			return false
		}
	case http.MethodDelete:
		if request.Header.Get(mcpSessionHeader) == "" {
			http.Error(writer, "Bad Request: DELETE requires an Mcp-Session-Id header", http.StatusBadRequest)

			return false
		}
	default:
		writer.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(writer, "Method Not Allowed", http.StatusMethodNotAllowed)

		return false
	}

	return true
}

// streamableAccepts reports whether the Accept values allow JSON and an
// event stream, with the go-sdk handler's rules.
func streamableAccepts(values []string) (jsonOK, streamOK bool) {
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			base, _, _ := strings.Cut(strings.TrimSpace(raw), ";")
			switch strings.ToLower(strings.TrimSpace(base)) {
			case "application/json", "application/*":
				jsonOK = true
			case "text/event-stream", "text/*":
				streamOK = true
			case "*/*":
				jsonOK, streamOK = true, true
			}
		}
	}

	return jsonOK, streamOK
}

// connectCopy connects a copy of the MCP session id on a new server. A nil
// state starts a session that the client initializes on this request.
func (e *mcpEndpoint) connectCopy(ctx context.Context, id string, binding mcpBinding, current requestContext, state *mcp.ServerSessionState) (*mcpSession, error) {
	server := e.newServer(false)
	transport := &mcp.StreamableServerTransport{SessionID: id}

	// The copy outlives the request that made it, so it keeps only that
	// request's trace span: the go-sdk hands the connection's context to
	// every handler, and the audit events and spans of the session's
	// calls join the trace of the request that made the copy.
	connectCtx := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx))

	serverSession, err := server.Connect(connectCtx, e.router.transport(transport), &mcp.ServerSessionOptions{State: state})
	if err != nil {
		return nil, fmt.Errorf("connect MCP session: %w", err)
	}

	held := &mcpSession{
		binding: binding, tools: newSessionTools(server, e.callTool, current),
		transport: transport, serverSession: serverSession,
	}

	// The go-sdk closes the session on a transport failure too, so the
	// copy is forgotten whichever way it closes.
	go func() {
		_ = serverSession.Wait()

		e.mu.Lock()
		defer e.mu.Unlock()

		if e.bindings[id] == held {
			e.forget(id, held)
		}
	}()

	return held, nil
}

// startSession makes the copy of a new MCP session for an initialize and
// marks a request on it as in progress. The initialize middleware stores
// its row.
func (e *mcpEndpoint) startSession(ctx context.Context, id string, binding mcpBinding, current requestContext) (*mcpSession, error) {
	held, err := e.connectCopy(ctx, id, binding, current, nil)
	if err != nil {
		return nil, err
	}

	held.active = 1
	held.lastTouch = time.Now()

	e.mu.Lock()
	e.bindings[id] = held
	e.mu.Unlock()

	return held, nil
}

// acquireOrAdopt returns this replica's copy of the MCP session id bound to
// binding, adopting it from its row when this replica holds none, and marks
// a request on it as in progress. It returns errMCPSessionNotFound for a
// session that doesn't exist, ended, or is bound to another identity.
func (e *mcpEndpoint) acquireOrAdopt(ctx context.Context, id string, binding mcpBinding) (*mcpSession, error) {
	for {
		e.mu.Lock()

		if held, found := e.bindings[id]; found {
			defer e.mu.Unlock()

			if held.binding != binding {
				return nil, errMCPSessionNotFound
			}

			held.active++

			return held, nil
		}

		if pending, found := e.adopting[id]; found {
			e.mu.Unlock()

			select {
			case <-pending.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		pending := &adoption{done: make(chan struct{})}
		e.adopting[id] = pending
		e.mu.Unlock()

		held, err := e.adopt(ctx, id, binding)

		e.mu.Lock()
		delete(e.adopting, id)

		if err == nil {
			held.active = 1
			held.lastTouch = time.Now()
			e.bindings[id] = held
		}

		e.mu.Unlock()
		close(pending.done)

		return held, err
	}
}

// adopt builds a copy of the MCP session id from its row, after it checked
// that the session is live and bound to binding.
func (e *mcpEndpoint) adopt(ctx context.Context, id string, binding mcpBinding) (*mcpSession, error) {
	if e.database == nil {
		return nil, errMCPSessionNotFound
	}

	readCtx, cancel := context.WithTimeout(ctx, mcpDatabaseTimeout)
	defer cancel()

	row, err := db.New(e.database).GetMCPSession(readCtx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errMCPSessionNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("read MCP session: %w", err)
	}

	stored := mcpBinding{principal: row.Principal, session: row.SessionID, workspace: row.Workspace, attestation: row.Attestation}
	if row.EndedAt.Valid || stored != binding {
		return nil, errMCPSessionNotFound
	}

	state := &mcp.ServerSessionState{
		InitializeParams:          &mcp.InitializeParams{},
		InitializedParams:         &mcp.InitializedParams{},
		NegotiatedProtocolVersion: row.ProtocolVersion,
	}

	if err := json.Unmarshal(row.InitializeParams, state.InitializeParams); err != nil {
		return nil, fmt.Errorf("decode initialize params: %w", err)
	}

	// The initialized notification can reach another replica after the
	// next request, so a row without it still serves.
	if len(row.InitializedParams) > 0 && string(row.InitializedParams) != "null" {
		if err := json.Unmarshal(row.InitializedParams, state.InitializedParams); err != nil {
			return nil, fmt.Errorf("decode initialized params: %w", err)
		}
	}

	return e.connectCopy(ctx, id, binding, requestContext{}, state)
}

// touch moves last_seen_at of the MCP session forward at most once a
// minute on this replica. It returns errMCPSessionNotFound when the row
// ended, after it closed the copy.
func (e *mcpEndpoint) touch(ctx context.Context, id string, held *mcpSession) error {
	e.mu.Lock()
	due := time.Since(held.lastTouch) >= touchInterval
	e.mu.Unlock()

	if !due || e.database == nil {
		return nil
	}

	touchCtx, cancel := context.WithTimeout(ctx, mcpDatabaseTimeout)
	defer cancel()

	touched, err := db.New(e.database).TouchMCPSession(touchCtx, id)
	if err != nil {
		e.logger.ErrorContext(ctx, "touching an MCP session failed", "error", err)

		return nil
	}

	if touched == 0 {
		e.closeCopy(id)

		return errMCPSessionNotFound
	}

	e.mu.Lock()
	held.lastTouch = time.Now()
	e.mu.Unlock()

	return nil
}

// touchOpenSessions touches, once a minute, every copy with a request or a
// GET stream in progress, so a long stream keeps its session live. It runs
// for the life of the process.
func (e *mcpEndpoint) touchOpenSessions() {
	ticker := time.NewTicker(touchInterval)
	defer ticker.Stop()

	for range ticker.C {
		e.mu.Lock()

		open := make(map[string]*mcpSession)

		for id, held := range e.bindings {
			if held.active > 0 {
				open[id] = held
			}
		}

		e.mu.Unlock()

		for id, held := range open {
			_ = e.touch(context.Background(), id, held)
		}
	}
}

// deleteSession ends the MCP session at a client's DELETE: one transaction
// sets ended_at, notifies every replica, and ends a connector's toolgate
// session. Then it closes this replica's copy.
func (e *mcpEndpoint) deleteSession(ctx context.Context, writer http.ResponseWriter, id string) {
	if e.database != nil {
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endSessionTimeout)
		defer cancel()

		if err := e.endMCPSession(endCtx, id); err != nil {
			e.logger.ErrorContext(ctx, "ending an MCP session failed", "error", err)
			http.Error(writer, "The gateway can't reach its database. Try again later.", http.StatusServiceUnavailable)

			return
		}
	}

	e.closeCopy(id)
	writer.WriteHeader(http.StatusNoContent)
}

// closeCopy forgets this replica's copy of the MCP session id, ends its GET
// streams and closes it once the requests in progress on it end, so a call
// waiting on the session still answers. The session's row is untouched.
func (e *mcpEndpoint) closeCopy(id string) {
	e.mu.Lock()

	held, found := e.bindings[id]
	if !found {
		e.mu.Unlock()

		return
	}

	e.forget(id, held)
	held.closing = true
	idle := held.active == 0

	for _, stream := range e.streams {
		if stream.session == held {
			stream.cancel()
		}
	}

	e.mu.Unlock()

	if idle {
		_ = held.serverSession.Close()
	}
}

// closeEndedCopies closes the copies whose row ended or is gone. It runs
// after missed notifications, when an mcpsession notification may have
// been lost.
func (e *mcpEndpoint) closeEndedCopies() {
	if e.database == nil {
		return
	}

	e.mu.Lock()

	ids := make([]string, 0, len(e.bindings))
	for id := range e.bindings {
		ids = append(ids, id)
	}

	e.mu.Unlock()

	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), mcpDatabaseTimeout)
		row, err := db.New(e.database).GetMCPSession(ctx, id)

		cancel()

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			e.closeCopy(id)
		case err != nil:
			e.logger.Error("reading an MCP session failed", "error", err)
		case row.EndedAt.Valid:
			e.closeCopy(id)
		}
	}
}

// storeSessions is a receiving middleware that writes the mcp_sessions row:
// at initialize, after the go-sdk made the result and before the result
// goes out, so the client's next request finds the row on any replica; and
// the params of notifications/initialized.
func (e *mcpEndpoint) storeSessions(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)

		switch {
		case err != nil:
		case method == initializeMethod:
			if storeErr := e.storeInitialize(ctx, request, result); storeErr != nil {
				e.logger.ErrorContext(ctx, "storing an MCP session failed", "error", storeErr)

				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "The gateway can't start a session right now. Try again later."}
			}
		case method == initializedMethod:
			if storeErr := e.storeInitialized(ctx, request); storeErr != nil {
				e.logger.ErrorContext(ctx, "storing the initialized params of an MCP session failed", "error", storeErr)
			}
		}

		return result, err
	}
}

// initializedMethod is the notification that ends the MCP handshake.
const initializedMethod = "notifications/initialized"

func (e *mcpEndpoint) storeInitialize(ctx context.Context, request mcp.Request, result mcp.Result) error {
	id := request.GetSession().ID()

	initialized, ok := result.(*mcp.InitializeResult)
	if !ok {
		return fmt.Errorf("initialize returned %T", result)
	}

	var current requestContext
	if extra := request.GetExtra(); extra != nil && extra.TokenInfo != nil {
		current, _ = extra.TokenInfo.Extra[requestContextKey].(requestContext)
	}

	if current.session.ID == 0 {
		return errors.New("initialize without a session")
	}

	params, err := json.Marshal(request.GetParams())
	if err != nil {
		return fmt.Errorf("encode initialize params: %w", err)
	}

	if e.database != nil {
		insertCtx, cancel := context.WithTimeout(ctx, mcpDatabaseTimeout)
		defer cancel()

		identity := current.identity

		_, err = db.New(e.database).InsertMCPSession(insertCtx, db.InsertMCPSessionParams{
			ID: id, SessionPk: current.session.ID,
			Principal: identity.Principal, SessionID: identity.Session,
			Workspace: identity.Workspace, Attestation: identity.Attestation,
			ProtocolVersion: initialized.ProtocolVersion, InitializeParams: params,
		})
		if err != nil {
			return fmt.Errorf("insert MCP session: %w", err)
		}
	}

	e.mu.Lock()
	if held, found := e.bindings[id]; found {
		held.stored = true
	}
	e.mu.Unlock()

	return nil
}

func (e *mcpEndpoint) storeInitialized(ctx context.Context, request mcp.Request) error {
	if e.database == nil {
		return nil
	}

	params, err := json.Marshal(request.GetParams())
	if err != nil {
		return fmt.Errorf("encode initialized params: %w", err)
	}

	updateCtx, cancel := context.WithTimeout(ctx, mcpDatabaseTimeout)
	defer cancel()

	_, err = db.New(e.database).SetMCPSessionInitializedParams(updateCtx, db.SetMCPSessionInitializedParamsParams{
		ID: request.GetSession().ID(), InitializedParams: params,
	})

	return err
}

// openStream is a GET stream in progress and the copy it is served on.
type openStream struct {
	session *mcpSession
	cancel  context.CancelFunc
}

// trackStream registers the context cancel of a GET stream on held, so
// Shutdown and closeCopy can end it.
func (e *mcpEndpoint) trackStream(held *mcpSession, cancel context.CancelFunc) func() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The session may have ended since the request acquired the copy.
	if held.closing {
		cancel()
	}

	e.nextStream++
	key := e.nextStream
	e.streams[key] = openStream{session: held, cancel: cancel}

	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()

		delete(e.streams, key)
	}
}

// Shutdown ends the GET streams of this replica's MCP sessions, so their
// clients reconnect to another replica. Requests in progress go on. The
// sessions stay live: another replica adopts them.
func (e *mcpEndpoint) Shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, stream := range e.streams {
		stream.cancel()
	}
}
