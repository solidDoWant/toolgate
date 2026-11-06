package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP protocol revisions toolgate chooses between for an upstream
// (docs/design/phase-4.md §8).
const (
	// RevisionStateful is the revision toolgate tries first.
	RevisionStateful = "2025-11-25"
	// RevisionStateless is the revision toolgate uses only with an upstream
	// that supports nothing older.
	RevisionStateless = "2026-07-28"
)

// codeRejectedByTransport is the code of the go-sdk's ErrRejected, which
// wraps transport failures that aren't answers from the server: HTTP 429
// and 5xx, and failed connections.
const codeRejectedByTransport = -32005

// ErrTransientVersionAnswer is the probe failure when the 2025-11-25
// handshake was refused but `server/discover` lists an older revision: the
// refusal was transient, and the next probe starts again from 2025-11-25.
var ErrTransientVersionAnswer = errors.New("the server refused MCP " + RevisionStateful +
	" but lists it in server/discover; the refusal was transient")

// errNoFallback stops the go-sdk from falling back to `initialize` on a
// session that must stay on 2026-07-28.
var errNoFallback = errors.New("the server doesn't speak MCP " + RevisionStateless)

// UpstreamOptions configures the sessions ConnectUpstream opens.
type UpstreamOptions struct {
	// Implementation names toolgate to the upstream. Nil means "toolgate".
	Implementation *mcp.Implementation
	// Client holds the go-sdk client options, such as handlers. Nil means
	// none.
	Client *mcp.ClientOptions
	// HTTPClient carries the requests. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// DisableStandaloneSSE leaves out the 2025-11-25 GET stream.
	DisableStandaloneSSE bool

	// listenEnded, when set, is called once the response stream of a
	// 2026-07-28 `subscriptions/listen` request ends.
	listenEnded func()
}

// ConnectUpstream opens an MCP session to an upstream instance on revision,
// and returns it with the revision it speaks. An empty revision, for a row
// that has none yet, runs the probe of docs/design/phase-4.md §8: it
// connects on 2025-11-25 and runs `tools/list`, and tries 2026-07-28 only
// after a version answer.
func ConnectUpstream(ctx context.Context, endpoint, revision string, options UpstreamOptions) (*mcp.ClientSession, string, error) {
	if revision != "" {
		session, err := connectOn(ctx, endpoint, revision, options, nil)
		if err != nil {
			return nil, "", err
		}

		return session, session.InitializeResult().ProtocolVersion, nil
	}

	first := &attempt{}

	session, err := connectOn(ctx, endpoint, RevisionStateful, options, first)
	duringConnect := err != nil

	if err == nil {
		_, err = session.ListTools(ctx, nil)
		if err == nil {
			return session, session.InitializeResult().ProtocolVersion, nil
		}

		if !first.versionAnswer(err, false) {
			// Any other tools/list failure is the caller's to see, on a
			// fresh session: the failed list can break this one.
			negotiated := session.InitializeResult().ProtocolVersion
			_ = session.Close()

			session, err = connectOn(ctx, endpoint, negotiated, options, nil)
			if err != nil {
				return nil, "", err
			}

			return session, negotiated, nil
		}

		_ = session.Close()
		err = fmt.Errorf("tools/list: %w", err)
	}

	if !first.versionAnswer(err, duringConnect) {
		return nil, "", fmt.Errorf("MCP %s: %w", RevisionStateful, err)
	}

	second := &attempt{}

	session, secondErr := connectOn(ctx, endpoint, RevisionStateless, options, second)
	if second.listsOlder() {
		if session != nil {
			_ = session.Close()
		}

		return nil, "", fmt.Errorf("MCP %s: %w: %w", RevisionStateful, ErrTransientVersionAnswer, err)
	}

	if secondErr == nil {
		if _, secondErr = session.ListTools(ctx, nil); secondErr == nil {
			return session, RevisionStateless, nil
		}

		_ = session.Close()
		secondErr = fmt.Errorf("tools/list: %w", secondErr)
	}

	return nil, "", fmt.Errorf("MCP %s: %w; MCP %s: %w", RevisionStateful, err, RevisionStateless, secondErr)
}

// connectOn connects one session on revision. A session on 2026-07-28 never
// falls back to `initialize`. A non-nil record sees every exchange.
func connectOn(ctx context.Context, endpoint, revision string, options UpstreamOptions, record *attempt) (*mcp.ClientSession, error) {
	implementation := options.Implementation
	if implementation == nil {
		implementation = &mcp.Implementation{Name: "toolgate", Version: "0"}
	}

	var clientOptions *mcp.ClientOptions

	if options.Client != nil {
		copied := *options.Client
		clientOptions = &copied
	}

	client := mcp.NewClient(implementation, clientOptions)

	stateless := revision >= RevisionStateless

	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if stateless && method == "initialize" {
				return nil, errNoFallback
			}

			result, err := next(ctx, method, request)
			if record != nil && err == nil {
				record.result(result)
			}

			return result, err
		}
	})

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	if record != nil || options.listenEnded != nil {
		recorded := *httpClient
		recorded.Transport = &statusRecorder{base: httpClient.Transport, record: record, listenEnded: options.listenEnded}
		httpClient = &recorded
	}

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: options.DisableStandaloneSSE,
	}, &mcp.ClientSessionOptions{ProtocolVersion: revision})
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", endpoint, err)
	}

	return session, nil
}

// exchange is one HTTP request of an attempt and the status it got.
type exchange struct {
	method string
	status int
}

// attempt records what one probe attempt saw: each HTTP exchange, a refused
// `initialize` revision, and the revisions `server/discover` listed.
type attempt struct {
	mu        sync.Mutex
	exchanges []exchange
	refused   bool
	older     bool
}

func (a *attempt) result(result mcp.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch typed := result.(type) {
	case *mcp.InitializeResult:
		if !slices.Contains(mcp.SupportedProtocolVersions(), typed.ProtocolVersion) {
			a.refused = true
		}
	case *mcp.DiscoverResult:
		for _, listed := range typed.SupportedVersions {
			if listed < RevisionStateless {
				a.older = true
			}
		}
	}
}

func (a *attempt) exchanged(method string, status int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.exchanges = append(a.exchanges, exchange{method: method, status: status})
}

func (a *attempt) listsOlder() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.older
}

// versionAnswer reports whether err, the failure of a 2025-11-25 attempt,
// is the server refusing that revision: a JSON-RPC error, HTTP 400, 404 or
// 405 to `initialize`, a refused `initialize` revision, or HTTP 400 to the
// first request after `initialize`. Connection errors, timeouts, HTTP 429
// and 5xx are not. duringConnect is true when the handshake itself failed.
func (a *attempt) versionAnswer(err error, duringConnect bool) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.refused {
		return true
	}

	for index, seen := range a.exchanges {
		if seen.status >= 200 && seen.status < 300 {
			continue
		}

		switch {
		case seen.status == http.StatusTooManyRequests || seen.status >= 500:
			return false
		case seen.method == "initialize":
			return seen.status == http.StatusBadRequest || seen.status == http.StatusNotFound ||
				seen.status == http.StatusMethodNotAllowed
		case index > 0 && a.exchanges[index-1].method == "initialize":
			return seen.status == http.StatusBadRequest
		}

		break
	}

	var wire *jsonrpc.Error

	return duringConnect && errors.As(err, &wire) && wire.Code != codeRejectedByTransport
}

// statusRecorder records the JSON-RPC method and HTTP status of each POST
// in record, and calls listenEnded when the response stream of a
// `subscriptions/listen` request ends. Either may be nil.
type statusRecorder struct {
	base        http.RoundTripper
	record      *attempt
	listenEnded func()
}

// RoundTrip implements http.RoundTripper.
func (r *statusRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	method := ""

	if request.Method == http.MethodPost && request.GetBody != nil && (r.record != nil || r.listenEnded != nil) {
		if body, err := request.GetBody(); err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(body, maxRecordedBody))
			_ = body.Close()

			if readErr == nil {
				if message, decodeErr := jsonrpc.DecodeMessage(bytes.TrimSpace(raw)); decodeErr == nil {
					if call, ok := message.(*jsonrpc.Request); ok {
						method = call.Method
					}
				}
			}
		}
	}

	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}

	response, err := base.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	if request.Method == http.MethodPost && r.record != nil {
		r.record.exchanged(method, response.StatusCode)
	}

	if method == "subscriptions/listen" && r.listenEnded != nil {
		if response.Body == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			r.listenEnded()
		} else {
			response.Body = &endSignalBody{ReadCloser: response.Body, ended: r.listenEnded}
		}
	}

	return response, nil
}

// endSignalBody calls ended once, when its body ends or is closed.
type endSignalBody struct {
	io.ReadCloser
	ended func()
	once  sync.Once
}

func (b *endSignalBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if err != nil {
		b.once.Do(b.ended)
	}

	return read, err
}

func (b *endSignalBody) Close() error {
	b.once.Do(b.ended)

	return b.ReadCloser.Close()
}

// maxRecordedBody bounds how much of a request body the recorder decodes.
// toolgate's own handshake requests are far smaller.
const maxRecordedBody = 64 << 10
