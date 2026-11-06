// Package mcprequest reads what the gateway routes an MCP request by: its
// protocol revision and its JSON-RPC method (docs/design/phase-4.md §8,
// "Client side: routing each request").
package mcprequest

import (
	"bytes"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const (
	// ProtocolVersionHeader carries the request's MCP revision.
	ProtocolVersionHeader = "Mcp-Protocol-Version"
	// MethodHeader carries the request's JSON-RPC method on 2026-07-28.
	MethodHeader = "Mcp-Method"
	// StatelessRevision is the first revision without an initialize
	// handshake. Requests on it, or on a later one, are served statelessly.
	StatelessRevision = "2026-07-28"
	// DiscoverMethod is the 2026-07-28 method that lists a server's
	// revisions. It needs no session on any revision.
	DiscoverMethod = "server/discover"
)

// MaxBodyBytes is the most of a request body Method reads to find the
// method. It matches the MCP endpoint's request limit.
const MaxBodyBytes = 4 << 20

// Stateless reports whether the request's Mcp-Protocol-Version is 2026-07-28
// or later. Revisions are dates, so they compare as strings.
func Stateless(request *http.Request) bool {
	return request.Header.Get(ProtocolVersionHeader) >= StatelessRevision
}

// MethodOf returns the JSON-RPC method of a request whose body is body: the
// Mcp-Method header, or else the method of the decoded body. When both are
// there and differ, it is empty, so a header can't claim another method
// than the body's. It is empty too for a body that isn't one JSON-RPC
// request and no header.
func MethodOf(request *http.Request, body []byte) string {
	header := request.Header.Get(MethodHeader)

	var decoded string

	if message, err := jsonrpc.DecodeMessage(body); err == nil {
		if call, ok := message.(*jsonrpc.Request); ok {
			decoded = call.Method
		}
	}

	switch {
	case header == "":
		return decoded
	case decoded == "" || decoded == header:
		return header
	default:
		return ""
	}
}

// Method returns the request's JSON-RPC method as MethodOf does. It reads at
// most MaxBodyBytes of the body and puts back what it read, so the next
// handler reads the whole body.
func Method(request *http.Request) string {
	if request.Body == nil || request.Method != http.MethodPost {
		return request.Header.Get(MethodHeader)
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, MaxBodyBytes+1))
	request.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), request.Body), request.Body}

	if err != nil || len(body) > MaxBodyBytes {
		return request.Header.Get(MethodHeader)
	}

	return MethodOf(request, body)
}

// IsDiscover reports whether the request is a server/discover.
func IsDiscover(request *http.Request) bool {
	return Method(request) == DiscoverMethod
}
