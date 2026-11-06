package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestLazyAuthenticatorWaitsForTheIdentityProvider(t *testing.T) {
	// The identity provider is reached through a proxy that answers 503
	// until it is switched on, like an IdP that starts after the gateway.
	var (
		up      atomic.Bool
		forward atomic.Pointer[httputil.ReverseProxy]
	)

	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !up.Load() {
			http.Error(writer, "starting", http.StatusServiceUnavailable)

			return
		}

		forward.Load().ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.Close)

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{Issuer: proxy.URL, Listener: listener})

	target, err := url.Parse("http://" + listener.Addr().String())
	require.NoError(t, err)
	forward.Store(httputil.NewSingleHostReverseProxy(target))

	authenticator := &lazyAuthenticator{
		ctx:     t.Context(),
		options: auth.Options{IssuerURL: proxy.URL, Audiences: []string{provider.Audience}},
		logger:  slog.New(slog.DiscardHandler),
	}

	handler := authenticator.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}), true)

	send := func(token string) int {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
		request.Header.Set(auth.SessionHeader, "session-1")

		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		return recorder.Code
	}

	token := provider.MintAccessToken(t, testutil.TokenClaims{Subject: "alice"})

	assert.Equal(t, http.StatusServiceUnavailable, send(token), "no request passes before the IdP is discovered")

	up.Store(true)

	assert.Equal(t, http.StatusNoContent, send(token), "a valid token passes once the IdP is reachable")
	assert.Equal(t, http.StatusUnauthorized, send(""), "a request without a token is refused")
}
