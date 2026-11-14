package main

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
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
	// registerMetrics, if set, registers the authenticator's metrics once
	// it is built.
	registerMetrics func(...prometheus.Collector) error

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

	if l.registerMetrics != nil {
		if err := l.registerMetrics(authenticator.Collectors()...); err != nil {
			l.logger.WarnContext(ctx, "cannot register the authenticator's metrics", "error", err)
		}
	}

	l.authenticator = authenticator

	return authenticator, nil
}

// Middleware is auth.Authenticator.Middleware once the identity provider
// was discovered; before that it refuses every request with 503.
func (l *lazyAuthenticator) Middleware(next http.Handler, mcp bool) http.Handler {
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

			handler = authenticator.Middleware(next, mcp)

			mu.Lock()
			guarded = handler
			mu.Unlock()
		}

		handler.ServeHTTP(writer, request)
	})
}

// refusalRecorder writes failed sign-ins as `auth.refuse` audit events.
type refusalRecorder struct {
	recorder *audit.Recorder
	logger   *slog.Logger
}

// RecordAuthRefusal writes refusal. Only a failed attestation carries a
// principal: the verified user's. A failed write is logged: the request is
// refused (or, for an attestation, served as the user) either way.
func (r refusalRecorder) RecordAuthRefusal(ctx context.Context, refusal auth.Refusal) {
	audience := refusal.Audience
	if audience == nil {
		audience = []string{}
	}

	event := audit.Event{
		Type:      audit.TypeAuthRefuse,
		Principal: refusal.Principal,
		Details: map[string]any{
			"reason":        refusal.Reason,
			"issuer":        refusal.Issuer,
			"audience":      audience,
			"remoteAddress": refusal.RemoteAddress,
		},
	}
	if refusal.TrustDomain != "" {
		event.Details["trustDomain"] = refusal.TrustDomain
	}

	if refusal.Outcome != "" {
		event.Details["outcome"] = refusal.Outcome
	}

	err := r.recorder.Record(ctx, event)
	if err != nil {
		r.logger.WarnContext(ctx, "cannot write the auth.refuse audit event", "error", err)
	}
}

// workspaceKeyOptions enables attestation with registered workspace keys
// when the workspaceKeys setting is on. Nil leaves it off.
func workspaceKeyOptions(cfg config.Config, server *gateway.Server) *auth.WorkspaceKeyOptions {
	if !cfg.WorkspaceKeys {
		return nil
	}

	return &auth.WorkspaceKeyOptions{
		Store:             workspaceKeyStore{keys: store.NewWorkspaceKeys(server.Database())},
		WorkspaceIDPrefix: cfg.WorkspaceKeysPrefix,
	}
}

// workspaceKeyStore adapts the database's key rows to the authenticator.
type workspaceKeyStore struct {
	keys *store.WorkspaceKeys
}

func (s workspaceKeyStore) WorkspaceKey(ctx context.Context, keyID string) (auth.WorkspaceKey, bool, error) {
	row, found, err := s.keys.WorkspaceKey(ctx, keyID)
	if err != nil || !found {
		return auth.WorkspaceKey{}, found, err
	}

	return auth.WorkspaceKey{
		ID: row.ID, Principal: row.Principal, Username: row.Username, Name: row.Name,
		Status: row.Status, PublicKey: ed25519.PublicKey(row.PublicKey), LastUsedAt: row.LastUsedAt,
	}, true, nil
}

func (s workspaceKeyStore) HasPendingKeyRequest(ctx context.Context, principal, keyID string) (bool, error) {
	return s.keys.HasPendingKeyRequest(ctx, principal, keyID)
}

func (s workspaceKeyStore) TouchWorkspaceKey(ctx context.Context, id int64) error {
	return s.keys.TouchWorkspaceKey(ctx, id)
}
