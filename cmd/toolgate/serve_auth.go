package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// discoveryTimeout bounds one attempt to read the identity provider's
// discovery document.
const discoveryTimeout = 10 * time.Second

// lazyAuthenticator builds the auth.Authenticator when it is first needed,
// so the gateway starts while its identity provider is unreachable. Until
// discovery succeeds, every request it guards is refused with 503; each
// such request tries discovery again.
type lazyAuthenticator struct {
	// ctx outlives every request: the authenticator fetches signing keys
	// with it.
	ctx     context.Context
	options auth.Options
	logger  *slog.Logger

	mu            sync.Mutex
	authenticator *auth.Authenticator
}

// get returns the authenticator, discovering the identity provider if no
// earlier attempt succeeded.
func (l *lazyAuthenticator) get() (*auth.Authenticator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.authenticator != nil {
		return l.authenticator, nil
	}

	ctx, cancel := context.WithTimeout(l.ctx, discoveryTimeout)
	defer cancel()

	authenticator, err := auth.New(ctx, l.options)
	if err != nil {
		return nil, err
	}

	l.authenticator = authenticator

	return authenticator, nil
}

// Middleware is auth.Authenticator.Middleware once the identity provider
// was discovered; before that it refuses every request with 503.
func (l *lazyAuthenticator) Middleware(next http.Handler, requireSession bool) http.Handler {
	var (
		mu      sync.Mutex
		guarded http.Handler
	)

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		handler := guarded
		mu.Unlock()

		if handler == nil {
			authenticator, err := l.get()
			if err != nil {
				l.logger.WarnContext(request.Context(), "cannot reach the identity provider", "error", err)
				http.Error(writer, "The gateway can't reach its identity provider. Try again later.", http.StatusServiceUnavailable)

				return
			}

			handler = authenticator.Middleware(next, requireSession)

			mu.Lock()
			guarded = handler
			mu.Unlock()
		}

		handler.ServeHTTP(writer, request)
	})
}
