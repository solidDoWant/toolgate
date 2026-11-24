package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requestIDPrefixLength is the length of a copy's request ID prefix: 8
// random bytes in lowercase hex.
const requestIDPrefixLength = 16

// requestIDSeparator joins a copy's prefix and the ID the go-sdk gave a
// request.
const requestIDSeparator = "-"

// newRequestIDPrefix returns a new prefix from crypto/rand, which never
// fails.
func newRequestIDPrefix() string {
	random := make([]byte, requestIDPrefixLength/2)
	_, _ = rand.Read(random)

	return hex.EncodeToString(random)
}

// prefixedTransport is an mcp.Transport whose connection gives every request
// it sends a prefix of its own (prefixedConnection).
type prefixedTransport struct {
	mcp.Transport

	// protocolVersion receives the version of the initialize result, for
	// the HTTP requests sent after it.
	protocolVersion *atomic.Pointer[string]
}

func (t *prefixedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return &prefixedConnection{
		Connection: connection, prefix: newRequestIDPrefix(), protocolVersion: t.protocolVersion,
	}, nil
}

// prefixedConnection changes the ID of each request an adopted copy sends
// to `<prefix>-<original id>`, with a random prefix per copy, and changes it
// back on the server's response. Copies of one upstream session on several
// replicas, or one after another on one replica, number their requests on
// their own from 1. Without the prefix, two of them could send the same ID
// at once, and the server refuses a request whose ID is already in flight
// on the session. The copy that initialized the session sends numbers,
// which never equal a prefixed ID. A `notifications/cancelled` names the
// request by its changed ID. The go-sdk still writes and parses every
// message.
//
// Wrapping the go-sdk's connection hides the initialize result from it, so
// the connection reads the result's protocol version itself and hands it
// to forwardingTransport, which sends it as the go-sdk would.
type prefixedConnection struct {
	mcp.Connection

	prefix          string
	protocolVersion *atomic.Pointer[string]

	mu         sync.Mutex
	initialize *jsonrpc.ID
}

func (c *prefixedConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		return c.Connection.Write(ctx, message)
	}

	if !request.IsCall() {
		if request.Method == "notifications/cancelled" {
			return c.Connection.Write(ctx, c.prefixCancelled(request))
		}

		return c.Connection.Write(ctx, message)
	}

	number, ok := request.ID.Raw().(int64)
	if !ok {
		return c.Connection.Write(ctx, message)
	}

	id, err := jsonrpc.MakeID(c.prefixed(number))
	if err != nil {
		return c.Connection.Write(ctx, message)
	}

	prefixed := *request
	prefixed.ID = id

	if request.Method == "initialize" {
		c.mu.Lock()
		c.initialize = &prefixed.ID
		c.mu.Unlock()
	}

	return c.Connection.Write(ctx, &prefixed)
}

func (c *prefixedConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.Connection.Read(ctx)
	if err != nil {
		return nil, err
	}

	response, ok := message.(*jsonrpc.Response)
	if !ok {
		return message, nil
	}

	id, ok := response.ID.Raw().(string)
	if !ok {
		return message, nil
	}

	original, found := strings.CutPrefix(id, c.prefix+requestIDSeparator)
	if !found {
		return message, nil
	}

	number, err := strconv.ParseInt(original, 10, 64)
	if err != nil {
		return message, nil
	}

	// MakeID takes a number as JSON decodes it, a float64.
	restoredID, err := jsonrpc.MakeID(float64(number))
	if err != nil {
		return message, nil
	}

	c.noteInitializeResult(response)

	restored := *response
	restored.ID = restoredID

	return &restored, nil
}

func (c *prefixedConnection) prefixed(number int64) string {
	return c.prefix + requestIDSeparator + strconv.FormatInt(number, 10)
}

// prefixCancelled returns request, a `notifications/cancelled`, naming the
// changed ID of the request it cancels.
func (c *prefixedConnection) prefixCancelled(request *jsonrpc.Request) *jsonrpc.Request {
	var params mcp.CancelledParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return request
	}

	var number int64

	switch raw := params.RequestID.(type) {
	case float64:
		number = int64(raw)
	case int64:
		number = raw
	default:
		return request
	}

	params.RequestID = c.prefixed(number)

	encoded, err := json.Marshal(&params)
	if err != nil {
		return request
	}

	prefixed := *request
	prefixed.Params = encoded

	return &prefixed
}

// noteInitializeResult hands the protocol version of the initialize result
// in response, if it is one, to the HTTP requests that follow.
func (c *prefixedConnection) noteInitializeResult(response *jsonrpc.Response) {
	c.mu.Lock()
	isInitialize := c.initialize != nil && *c.initialize == response.ID
	c.mu.Unlock()

	if !isInitialize || response.Error != nil || c.protocolVersion == nil {
		return
	}

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	if err := json.Unmarshal(response.Result, &result); err != nil || result.ProtocolVersion == "" {
		return
	}

	c.protocolVersion.Store(&result.ProtocolVersion)
}
