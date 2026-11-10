// Package gateway runs the toolgate gateway's HTTP listeners: the main port
// (requests and health probes) and the separate metrics port.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/health"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/telemetry"
	"github.com/solidDoWant/toolgate/internal/wellknown"
)

// Bounds on what a client can send before a handler runs.
const (
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 64 << 10
)

// Server is the gateway. Create it with New, bind its ports with Listen and
// run it with Serve.
type Server struct {
	cfg    config.Config
	logger *slog.Logger

	mux            *http.ServeMux
	publicRoutes   []string
	database       *pgxpool.Pool
	tracerProvider *sdktrace.TracerProvider
	metrics        *prometheus.Registry

	listener        net.Listener
	metricsListener net.Listener

	onShutdown []func()
}

// New builds the gateway from cfg. It does not connect to PostgreSQL: the
// connection pool connects on first use, and `/readyz` fails until it can.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	// store.Open re-reads the TLS client certificate files for every new
	// connection, so a rotated certificate is used once the pool recycles
	// its connections.
	database, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		// The error can echo the connection string, so it is not wrapped.
		return nil, errors.New("database: cannot create the PostgreSQL connection pool")
	}

	tracerProvider, err := telemetry.NewTracerProvider(ctx, cfg.OTLPEndpointFor("traces"))
	if err != nil {
		database.Close()

		return nil, err
	}

	server := &Server{
		cfg:            cfg,
		logger:         logger,
		mux:            http.NewServeMux(),
		database:       database,
		tracerProvider: tracerProvider,
		metrics:        newMetricsRegistry(),
	}

	server.metrics.MustRegister(opsmetrics.PoolCollector(database))

	health.Register(server.mux, logger, health.Check{Name: "postgresql", Run: database.Ping})
	server.publicRoutes = append(server.publicRoutes, "GET /healthz", "GET /readyz")

	server.HandlePublic("GET "+wellknown.Path, wellknown.Handler(wellknown.ClientMetadata{
		Issuer:     cfg.OIDCIssuerURL,
		ClientID:   cfg.OIDCCLIClientID,
		Scopes:     cfg.OIDCCLIScopes,
		EmailClaim: cfg.OIDCEmailClaim,
	}))

	if resource := cfg.MCPResourceURL(); resource != "" {
		// One handler, so both paths share one rate limit per address.
		metadata := wellknown.ProtectedResourceHandler(wellknown.ProtectedResource{
			Resource: resource,
			Issuer:   cfg.OIDCIssuerURL,
			Scopes:   cfg.OIDCCLIScopes,
		})
		server.HandlePublic("GET "+wellknown.ProtectedResourcePath, metadata)
		server.HandlePublic("GET "+wellknown.ProtectedResourceRootPath, metadata)
	}

	return server, nil
}

// HandlePublic registers handler for pattern on the main port without
// authentication. Only the probes, the client metadata document,
// the protected resource metadata
// and the web UI's sign-in routes and static
// files are public.
func (s *Server) HandlePublic(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
	s.publicRoutes = append(s.publicRoutes, pattern)
}

// PublicRoutes returns the patterns served on the main port without
// authentication.
func (s *Server) PublicRoutes() []string {
	return slices.Clone(s.publicRoutes)
}

// Handle registers handler for pattern on the main port. The handler must
// authenticate the request itself.
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

// RegisterOnShutdown registers f to run when the main port starts to shut
// down, as http.Server.RegisterOnShutdown. Call it before Serve.
func (s *Server) RegisterOnShutdown(f func()) {
	s.onShutdown = append(s.onShutdown, f)
}

// Database returns the gateway's PostgreSQL connection pool. Serve closes
// it.
func (s *Server) Database() *pgxpool.Pool {
	return s.database
}

// Listen binds the main and metrics ports.
func (s *Server) Listen() error {
	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(context.Background(), "tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listenAddress: %w", err)
	}

	metricsListener, err := listenConfig.Listen(context.Background(), "tcp", s.cfg.MetricsListenAddress)
	if err != nil {
		_ = listener.Close()

		return fmt.Errorf("metricsListenAddress: %w", err)
	}

	s.listener = listener
	s.metricsListener = metricsListener

	return nil
}

// Addr returns the bound address of the main port. Call it after Listen.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// TracerProvider returns the provider of the gateway's spans, which exports
// them to the configured OTLP endpoint.
func (s *Server) TracerProvider() trace.TracerProvider {
	return s.tracerProvider
}

// MetricsAddr returns the bound address of the metrics port. Call it after
// Listen.
func (s *Server) MetricsAddr() net.Addr {
	return s.metricsListener.Addr()
}

// Serve serves both ports until ctx is cancelled, then shuts down: it stops
// accepting connections, waits up to the configured shutdown timeout for
// in-flight requests, flushes spans and closes the database pool. It returns
// an error when a listener fails or the requests did not drain in time.
func (s *Server) Serve(ctx context.Context) error {
	defer s.database.Close()

	mainServer := &http.Server{
		Handler: otelhttp.NewHandler(s.mux, "gateway",
			otelhttp.WithTracerProvider(s.tracerProvider),
			otelhttp.WithPropagators(telemetry.Propagator()),
			otelhttp.WithFilter(traced),
			otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
				return request.Method + " " + request.URL.Path
			}),
		),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}

	for _, f := range s.onShutdown {
		mainServer.RegisterOnShutdown(f)
	}

	metricsServer := &http.Server{
		Handler:           metricsHandler(s.metrics),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}

	serveErrors := make(chan error, 2)

	go func() { serveErrors <- mainServer.Serve(s.listener) }()
	go func() { serveErrors <- metricsServer.Serve(s.metricsListener) }()

	s.logger.InfoContext(ctx, "gateway started",
		"address", s.listener.Addr().String(),
		"metricsAddress", s.metricsListener.Addr().String())

	var serveErr error

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutting down", "timeout", s.cfg.ShutdownTimeout.String())
	case err := <-serveErrors:
		serveErr = fmt.Errorf("serve: %w", err)
		s.logger.ErrorContext(ctx, "listener failed, shutting down", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(s.cfg.ShutdownTimeout))
	defer cancel()

	shutdownErrors := []error{serveErr}

	if err := mainServer.Shutdown(shutdownCtx); err != nil {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("requests did not drain within %s: %w", s.cfg.ShutdownTimeout, err))

		_ = mainServer.Close()
	}

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		_ = metricsServer.Close()
	}

	// Spans get their own short budget, so a slow drain still flushes them.
	flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelFlush()

	if err := s.tracerProvider.Shutdown(flushCtx); err != nil {
		s.logger.WarnContext(ctx, "flushing spans failed", "error", err)
	}

	return errors.Join(shutdownErrors...)
}

// OTLPIngestPathPrefix prefixes the OTLP ingest routes.
const OTLPIngestPathPrefix = "/v1/"

// traced reports whether the gateway traces request. The OTLP ingest routes
// are not traced: each export would make gateway spans, which would be
// exported in turn.
func traced(request *http.Request) bool {
	return !strings.HasPrefix(request.URL.Path, OTLPIngestPathPrefix)
}

// newMetricsRegistry returns the registry served at /metrics, holding the
// process metrics, the basic Go memory and garbage collector metrics (not
// the full runtime list), the build info and the request counters.
func newMetricsRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		opsmetrics.BuildInfo(),
	)
	registry.MustRegister(opsmetrics.Collectors()...)

	return registry
}

// RegisterMetrics adds collectors to the metrics served at /metrics.
func (s *Server) RegisterMetrics(collectors ...prometheus.Collector) error {
	for _, collector := range collectors {
		if err := s.metrics.Register(collector); err != nil {
			return fmt.Errorf("register metrics: %w", err)
		}
	}

	return nil
}

// metricsHandler serves the Prometheus metrics at /metrics.
func metricsHandler(registry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	return mux
}
