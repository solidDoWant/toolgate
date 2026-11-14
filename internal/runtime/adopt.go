package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// sessionIDHeader is the Streamable HTTP header that carries the upstream
// session ID.
const sessionIDHeader = "Mcp-Session-Id"

// errAdoptNeedsSessions refuses Adopt on a revision that has no sessions.
var errAdoptNeedsSessions = errors.New("an adopted upstream session needs an MCP revision before " + RevisionStateless)

// AdoptedSession is an upstream session another connection already
// initialized: the server's session ID and its `initialize` result.
type AdoptedSession struct {
	// ID is the server's Mcp-Session-Id.
	ID string
	// InitializeResult is the server's InitializeResult as JSON.
	InitializeResult json.RawMessage
}

// SessionNotFound records that the server answered a request of a call with
// HTTP 404 for its session. The caller puts it in the call's context with
// WithSessionNotFound. The go-sdk recognizes only a 404 without a JSON-RPC
// error body as a lost session, so this flag catches the others.
type SessionNotFound struct {
	found atomic.Bool
}

// Found reports whether a request of the call got HTTP 404 for its session.
func (s *SessionNotFound) Found() bool {
	return s.found.Load()
}

type sessionNotFoundKey struct{}

// WithSessionNotFound returns ctx carrying flag. Requests sent with the
// returned context set flag when the server answers HTTP 404 for their
// session.
func WithSessionNotFound(ctx context.Context, flag *SessionNotFound) context.Context {
	return context.WithValue(ctx, sessionNotFoundKey{}, flag)
}

// forwardingTransport carries the requests of a forwarding connection. It
// answers `initialize` and `notifications/initialized` itself when adopt is
// set, answers the closing DELETE itself when deleteOnClose is set and
// false, and sets the SessionNotFound flag of a request's context on a 404
// for its session. Every other request goes to base unchanged.
type forwardingTransport struct {
	base          http.RoundTripper
	adopt         *AdoptedSession
	deleteOnClose *atomic.Bool
}

// RoundTrip implements http.RoundTripper.
func (f *forwardingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodDelete && f.deleteOnClose != nil && !f.deleteOnClose.Load() {
		closeBody(request)

		return localResponse(request, http.StatusNoContent, nil, nil), nil
	}

	if f.adopt != nil && request.Method == http.MethodPost {
		if response, handled := f.answerHandshake(request); handled {
			return response, nil
		}
	}

	base := f.base
	if base == nil {
		base = http.DefaultTransport
	}

	response, err := base.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	if response.StatusCode == http.StatusNotFound && request.Header.Get(sessionIDHeader) != "" {
		if flag, ok := request.Context().Value(sessionNotFoundKey{}).(*SessionNotFound); ok && flag != nil {
			flag.found.Store(true)
		}
	}

	return response, nil
}

// answerHandshake answers an `initialize` request with the adopted session
// and a `notifications/initialized` notification with 202, and reports
// whether request was one of them.
func (f *forwardingTransport) answerHandshake(request *http.Request) (*http.Response, bool) {
	if request.GetBody == nil {
		return nil, false
	}

	body, err := request.GetBody()
	if err != nil {
		return nil, false
	}

	raw, err := io.ReadAll(io.LimitReader(body, maxRecordedBody))
	_ = body.Close()

	if err != nil {
		return nil, false
	}

	message, err := jsonrpc.DecodeMessage(bytes.TrimSpace(raw))
	if err != nil {
		return nil, false
	}

	call, ok := message.(*jsonrpc.Request)
	if !ok {
		return nil, false
	}

	switch call.Method {
	case "initialize":
		encoded, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: call.ID, Result: f.adopt.InitializeResult})
		if err != nil {
			return nil, false
		}

		closeBody(request)

		header := http.Header{}
		header.Set("Content-Type", "application/json")
		header.Set(sessionIDHeader, f.adopt.ID)

		return localResponse(request, http.StatusOK, header, encoded), true
	case "notifications/initialized":
		closeBody(request)

		return localResponse(request, http.StatusAccepted, nil, nil), true
	}

	return nil, false
}

// closeBody closes a request's body, as a RoundTripper must even when it
// sends nothing.
func closeBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}

// localResponse is a response made without the network.
func localResponse(request *http.Request, status int, header http.Header, body []byte) *http.Response {
	if header == nil {
		header = http.Header{}
	}

	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}
