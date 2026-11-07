package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionTransport gives the connections of a transport an MCP session ID,
// as a Streamable HTTP transport has.
type sessionTransport struct {
	mcp.Transport

	id string
}

func (t sessionTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return sessionConnection{Connection: connection, id: t.id}, nil
}

type sessionConnection struct {
	mcp.Connection

	id string
}

func (c sessionConnection) SessionID() string { return c.id }

// elsewhereTransport is a client transport whose responses don't go back on
// its own connection: answer receives them, as another replica would.
type elsewhereTransport struct {
	mcp.Transport

	answer func(*jsonrpc.Response)
	// seen records the IDs of the requests the client received.
	seen func(jsonrpc.ID)
}

func (t elsewhereTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return elsewhereConnection{Connection: connection, transport: t}, nil
}

type elsewhereConnection struct {
	mcp.Connection

	transport elsewhereTransport
}

func (c elsewhereConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.Connection.Read(ctx)
	if request, ok := message.(*jsonrpc.Request); ok && request.IsCall() {
		c.transport.seen(request.ID)
	}

	return message, err
}

func (c elsewhereConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if response, ok := message.(*jsonrpc.Response); ok {
		c.transport.answer(response)

		return nil
	}

	return c.Connection.Write(ctx, message)
}

// publish routes a response as the endpoint and the replies channel do:
// it names its replica, and every replica receives the payload.
func publish(t *testing.T, session string, routers ...*replyRouter) func(*jsonrpc.Response) {
	t.Helper()

	return func(response *jsonrpc.Response) {
		replica, isResponse := replyTarget(response)
		assert.True(t, isResponse)

		message, err := jsonrpc.EncodeMessage(response)
		require.NoError(t, err)

		payload, err := json.Marshal(routedReply{Replica: replica, Session: session, Message: message})
		require.NoError(t, err)

		for _, router := range routers {
			go router.deliver(payload)
		}
	}
}

// askingServer is a server with one tool that elicits and returns the
// answer's "said" field. It speaks 2025-11-25, the revision with MCP
// sessions and server requests.
func askingServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})

	server.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		answer, err := request.Session.Elicit(ctx, &mcp.ElicitParams{
			Message:         "Say something.",
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"said": map[string]any{"type": "string"}}},
		})
		if err != nil {
			return nil, err
		}

		said, _ := answer.Content["said"].(string)

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: said}}}, nil
	})

	return server
}

// connectCopy connects a copy of the MCP session on router's replica to a
// client whose answers go to answer, and returns the client session.
func connectCopy(t *testing.T, router *replyRouter, session string, said string, answer func(*jsonrpc.Response), seen func(jsonrpc.ID)) *mcp.ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := askingServer().Connect(t.Context(), router.transport(sessionTransport{Transport: serverTransport, id: session}), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "client"}, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"said": said}}, nil
		},
	})

	// The handshake's responses come from the server, so they never go
	// elsewhere; only the client's answers to server requests do.
	clientSession, err := client.Connect(t.Context(), elsewhereTransport{Transport: clientTransport, answer: answer, seen: seen}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

// callAsk calls the asking tool and returns its text, or the error text.
func callAsk(ctx context.Context, session *mcp.ClientSession) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ask"})
	if err != nil {
		return err.Error()
	}

	if len(result.Content) != 1 {
		return "no content"
	}

	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return "not text"
	}

	return text.Text
}

func TestElicitationAnsweredOnAnotherReplicaReachesTheWaitingCall(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	asking := newReplyRouter("aaaaaaaaaaaaaaaa", logger)
	other := newReplyRouter("bbbbbbbbbbbbbbbb", logger)

	var seen []jsonrpc.ID

	session := connectCopy(t, asking, "session", "hello", publish(t, "session", asking, other),
		func(id jsonrpc.ID) { seen = append(seen, id) })

	assert.Equal(t, "hello", callAsk(t.Context(), session))

	require.Len(t, seen, 1)
	id, ok := seen[0].Raw().(string)
	require.True(t, ok, "the client sees a string ID")
	assert.True(t, strings.HasPrefix(id, "aaaaaaaaaaaaaaaa-"), "the ID %q names the asking replica", id)
}

func TestTwoCopiesOfOneSessionGetDistinctIDsAndTheirOwnAnswers(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	first := newReplyRouter("aaaaaaaaaaaaaaaa", logger)
	second := newReplyRouter("bbbbbbbbbbbbbbbb", logger)

	var (
		mu   sync.Mutex
		seen []string
	)

	record := func(id jsonrpc.ID) {
		mu.Lock()
		defer mu.Unlock()

		raw, _ := id.Raw().(string)
		seen = append(seen, raw)
	}

	// Each copy's answer goes through the replies channel, reaching both
	// replicas, so only the prefix decides who takes it.
	onFirst := connectCopy(t, first, "session", "first", publish(t, "session", first, second), record)
	onSecond := connectCopy(t, second, "session", "second", publish(t, "session", first, second), record)

	answers := make([]string, 2)

	var calls sync.WaitGroup

	calls.Add(2)

	go func() {
		defer calls.Done()

		answers[0] = callAsk(t.Context(), onFirst)
	}()

	go func() {
		defer calls.Done()

		answers[1] = callAsk(t.Context(), onSecond)
	}()

	calls.Wait()

	assert.Equal(t, []string{"first", "second"}, answers)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, seen, 2)
	assert.NotEqual(t, seen[0], seen[1], "the two copies' requests have distinct IDs")
}
