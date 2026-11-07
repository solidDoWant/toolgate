package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/store"
)

// replicaIDLength is the length of a replica ID: 8 random bytes in
// lowercase hex.
const replicaIDLength = 16

// replicaIDSeparator joins a replica ID and the ID a copy gave its request.
const replicaIDSeparator = "-"

// newReplicaID returns a new replica ID from crypto/rand, which never
// fails.
func newReplicaID() string {
	random := make([]byte, replicaIDLength/2)
	_, _ = rand.Read(random)

	return hex.EncodeToString(random)
}

// isReplicaID reports whether value has the form of a replica ID.
func isReplicaID(value string) bool {
	if len(value) != replicaIDLength {
		return false
	}

	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}

	return true
}

// replyTarget returns the replica whose request message answers, and true,
// when message is a client's response. Two copies of one MCP session on two
// replicas number their requests on their own, so each request ID carries
// the replica that sent it. replica is empty when the ID names no replica.
func replyTarget(message jsonrpc.Message) (replica string, isResponse bool) {
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		return "", false
	}

	id, ok := response.ID.Raw().(string)
	if !ok {
		return "", true
	}

	prefix, _, found := strings.Cut(id, replicaIDSeparator)
	if !found || !isReplicaID(prefix) {
		return "", true
	}

	return prefix, true
}

// routedReply is the payload of a reply on store.RepliesChannel.
type routedReply struct {
	Replica string          `json:"replica"`
	Session string          `json:"session"`
	Message json.RawMessage `json:"message"`
}

// replyRouter keeps this replica's ID and the routed connections of the MCP
// session copies it holds, so a reply from another replica reaches the copy
// that asked.
type replyRouter struct {
	replica string
	logger  *slog.Logger

	mu     sync.Mutex
	copies map[string]*routedConnection
}

func newReplyRouter(replica string, logger *slog.Logger) *replyRouter {
	return &replyRouter{replica: replica, logger: logger, copies: make(map[string]*routedConnection)}
}

// transport wraps the transport of an MCP session copy, so the copy's
// requests carry this replica's ID and the client's responses find their
// way back.
func (r *replyRouter) transport(transport mcp.Transport) mcp.Transport {
	return &routedTransport{Transport: transport, router: r}
}

// deliver hands a reply that arrived on store.RepliesChannel to the copy
// that sent the request. A reply for another replica, or for a copy this
// replica no longer holds, is ignored.
func (r *replyRouter) deliver(payload []byte) {
	var reply routedReply
	if err := json.Unmarshal(payload, &reply); err != nil {
		r.logger.Warn("ignoring a malformed routed reply")

		return
	}

	if reply.Replica != r.replica {
		return
	}

	message, err := jsonrpc.DecodeMessage(reply.Message)
	if err != nil {
		r.logger.Warn("ignoring a routed reply that isn't a JSON-RPC message")

		return
	}

	response, ok := message.(*jsonrpc.Response)
	if !ok {
		r.logger.Warn("ignoring a routed reply that isn't a response")

		return
	}

	r.mu.Lock()
	connection, found := r.copies[reply.Session]
	r.mu.Unlock()

	if !found {
		return
	}

	connection.route(response)
}

// notifyToolListChanged sends one tools list_changed notification on the
// GET stream of the copy of the MCP session id, if this replica holds it.
func (r *replyRouter) notifyToolListChanged(id string) {
	r.mu.Lock()
	connection, found := r.copies[id]
	r.mu.Unlock()

	if !found {
		return
	}

	notification := &jsonrpc.Request{Method: toolListChangedMethod, Params: json.RawMessage(`{}`)}
	if err := connection.Connection.Write(context.Background(), notification); err != nil {
		r.logger.Warn("sending list_changed on an attached stream failed", slog.String("error", err.Error()))
	}
}

// toolListChangedMethod is the notification that tells a client to list
// tools again.
const toolListChangedMethod = "notifications/tools/list_changed"

func (r *replyRouter) register(connection *routedConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.copies[connection.SessionID()] = connection
}

func (r *replyRouter) unregister(connection *routedConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.copies[connection.SessionID()] == connection {
		delete(r.copies, connection.SessionID())
	}
}

// routedTransport is an mcp.Transport whose connection rewrites request IDs
// (routedConnection). It passes through the optional interfaces the go-sdk
// checks on a transport.
type routedTransport struct {
	mcp.Transport

	router *replyRouter
}

func (t *routedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	routed := &routedConnection{
		Connection: connection,
		router:     t.router,
		routed:     make(chan *jsonrpc.Response),
		closed:     make(chan struct{}),
	}

	if connection.SessionID() != "" {
		t.router.register(routed)
	}

	return routed, nil
}

// SupportsProtocolVersion answers as the wrapped transport does. A transport
// without the method serves every version.
func (t *routedTransport) SupportsProtocolVersion(version string) bool {
	if supporter, ok := t.Transport.(mcp.ProtocolVersionSupporter); ok {
		return supporter.SupportsProtocolVersion(version)
	}

	return true
}

// routedConnection changes the ID of each request the server sends to
// `<replica id>-<original id>`, and changes it back on the client's
// response. A response without this replica's prefix is dropped: the
// endpoint has already sent it to the replica it names, and any other ID
// could match a request of this copy by chance. The go-sdk still writes and
// parses every message.
type routedConnection struct {
	mcp.Connection

	router *replyRouter
	// routed carries the responses that reached another replica.
	routed chan *jsonrpc.Response

	pump      sync.Once
	incoming  chan readResult
	closeOnce sync.Once
	closed    chan struct{}
}

// errRoutedConnectionClosed is returned by a Read after Close.
var errRoutedConnectionClosed = errors.New("connection closed")

type readResult struct {
	message jsonrpc.Message
	err     error
}

// route feeds a response that reached another replica to the copy. It gives
// up when the connection closes.
func (c *routedConnection) route(response *jsonrpc.Response) {
	select {
	case c.routed <- response:
	case <-c.closed:
	}
}

func (c *routedConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	// The wrapped connection's Read is moved to one goroutine, so a
	// routed response can be read while it blocks. Close unblocks it.
	c.pump.Do(func() {
		c.incoming = make(chan readResult)

		go func() {
			for {
				message, err := c.Connection.Read(context.WithoutCancel(ctx))
				select {
				case c.incoming <- readResult{message: message, err: err}:
				case <-c.closed:
					return
				}

				if err != nil {
					return
				}
			}
		}()
	})

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, errRoutedConnectionClosed
		case response := <-c.routed:
			if restored, ok := c.restore(response); ok {
				return restored, nil
			}
		case result := <-c.incoming:
			if result.err != nil {
				return nil, result.err
			}

			response, isResponse := result.message.(*jsonrpc.Response)
			if !isResponse {
				return result.message, nil
			}

			if restored, ok := c.restore(response); ok {
				return restored, nil
			}

			c.router.logger.Warn("dropping a response to a request this replica didn't send")
		}
	}
}

// restore gives a response the ID of the request it answers, or returns
// false when the request wasn't sent by this replica.
func (c *routedConnection) restore(response *jsonrpc.Response) (*jsonrpc.Response, bool) {
	id, ok := response.ID.Raw().(string)
	if !ok {
		return nil, false
	}

	original, found := strings.CutPrefix(id, c.router.replica+replicaIDSeparator)
	if !found {
		return nil, false
	}

	// MakeID takes a number as JSON decodes it, a float64.
	var raw any = original
	if number, err := strconv.ParseInt(original, 10, 64); err == nil {
		raw = float64(number)
	}

	restoredID, err := jsonrpc.MakeID(raw)
	if err != nil {
		return nil, false
	}

	restored := *response
	restored.ID = restoredID

	return &restored, true
}

func (c *routedConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok || !request.IsCall() {
		return c.Connection.Write(ctx, message)
	}

	var original string

	switch raw := request.ID.Raw().(type) {
	case int64:
		original = strconv.FormatInt(raw, 10)
	case string:
		original = raw
	default:
		return errors.New("a server request has an ID that is neither a number nor a string")
	}

	rewrittenID, err := jsonrpc.MakeID(c.router.replica + replicaIDSeparator + original)
	if err != nil {
		return err
	}

	rewritten := *request
	rewritten.ID = rewrittenID

	return c.Connection.Write(ctx, &rewritten)
}

func (c *routedConnection) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.router.unregister(c)
	})

	return c.Connection.Close()
}

// routeReply sends a POST that carries a client's response to a request of
// another replica on to that replica, and answers 202 after the commit. A
// response that names no replica, or one too large for a notification, is
// dropped with a warning and also answered 202. It returns false for any
// other body, and for a response to this replica's own request, which the
// copy serves.
func (e *mcpEndpoint) routeReply(ctx context.Context, writer http.ResponseWriter, mcpSessionID string, body []byte) bool {
	message, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		return false
	}

	replica, isResponse := replyTarget(message)
	if !isResponse || replica == e.router.replica {
		return false
	}

	if replica == "" {
		e.logger.WarnContext(ctx, "dropping a response to a request no replica sent")
		writer.WriteHeader(http.StatusAccepted)

		return true
	}

	payload, err := json.Marshal(routedReply{Replica: replica, Session: mcpSessionID, Message: body})
	if err == nil {
		err = store.NotifyReply(ctx, e.database, payload)
	}

	switch {
	case errors.Is(err, store.ErrReplyTooLarge):
		e.logger.WarnContext(ctx, "dropping a response too large to send to another replica")
	case err != nil:
		e.logger.ErrorContext(ctx, "sending a response to another replica failed", slog.String("error", err.Error()))
		http.Error(writer, "The gateway can't reach its database. Try again later.", http.StatusServiceUnavailable)

		return true
	}

	writer.WriteHeader(http.StatusAccepted)

	return true
}

// streamAttachWriter calls attached once when a GET stream starts, which is
// when the go-sdk writes its 200 header with the stream registered.
type streamAttachWriter struct {
	http.ResponseWriter

	attached func()
	once     sync.Once
}

func (w *streamAttachWriter) WriteHeader(status int) {
	if status == http.StatusOK {
		w.once.Do(w.attached)
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *streamAttachWriter) Write(data []byte) (int, error) {
	return w.ResponseWriter.Write(data)
}

// Unwrap lets http.ResponseController reach the underlying writer, so the
// stream can be flushed.
func (w *streamAttachWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
