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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/health"
	"github.com/solidDoWant/toolgate/internal/telemetry"
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
	database       *pgxpool.Pool
	tracerProvider *sdktrace.TracerProvider

	listener        net.Listener
	metricsListener net.Listener
}

// New builds the gateway from cfg. It does not connect to PostgreSQL: the
// connection pool connects on first use, and `/readyz` fails until it can.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	database, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		// The error can echo the connection string, so it is not wrapped.
		return nil, errors.New("database: cannot create the PostgreSQL connection pool")
	}

	tracerProvider, err := telemetry.NewTracerProvider(ctx, cfg.OTLPEndpoint)
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
	}

	health.Register(server.mux, logger, health.Check{Name: "postgresql", Run: database.Ping})

	return server, nil
}

// Handle registers handler for pattern on the main port.
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
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
			otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
				return request.Method + " " + request.URL.Path
			}),
		),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}

	metricsServer := &http.Server{
		Handler:           metricsHandler(),
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

// metricsHandler serves the Prometheus metrics at /metrics.
func metricsHandler() http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	return mux
}
