package gateway

import (
	"context"
	"net/http"
)

// BughuntWrapEndSession wraps the end of a session tied to an MCP session.
func BughuntWrapEndSession(handler http.Handler, wrap func(next func(ctx context.Context, id int64, reason string) error) func(ctx context.Context, id int64, reason string) error) {
	endpoint := handler.(*mcpEndpoint)
	endpoint.endSession = wrap(endpoint.endSession)
}
