package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
)

// serve runs the gateway until SIGTERM or SIGINT and returns the exit code.
func serve(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", os.Getenv(config.FileEnv),
		"path to the YAML configuration file (default from "+config.FileEnv+")")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate serve: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	cfg, err := config.Load(*configPath, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate serve: invalid configuration:\n%s\n", err)

		return 1
	}

	logger := newLogger(stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	server, err := gateway.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	if cfg.SecretsKeysetFile != "" {
		if err := registerSecretsAPI(ctx, server, cfg, stdout, logger); err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}
	}

	if err := server.Listen(); err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	// Install the tracer provider's propagator process-wide, so outgoing
	// requests made with the global OpenTelemetry API carry trace context.
	telemetry.SetGlobalPropagator()

	// A replica that loses the Lease stops: its manager can't regain it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var runtimeResult chan error

	if cfg.LeaseNamespace != "" {
		managerDone, err := startRuntime(ctx, cfg, server.Database(), logger)
		if err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}

		runtimeResult = make(chan error, 1)

		go func() {
			runtimeResult <- <-managerDone

			cancel()
		}()
	}

	serveErr := server.Serve(ctx)

	cancel()

	if runtimeResult != nil {
		if err := <-runtimeResult; err != nil {
			logger.Error("runtime reconciler stopped with an error", "error", err)

			return 1
		}
	}

	if serveErr != nil {
		logger.Error("gateway stopped with an error", "error", serveErr)

		return 1
	}

	logger.Info("gateway stopped")

	return 0
}

// registerSecretsAPI serves `/api/v1/secrets` behind user authentication.
func registerSecretsAPI(ctx context.Context, server *gateway.Server, cfg config.Config, stdout io.Writer, logger *slog.Logger) error {
	cipher, err := secrets.LoadKeyset(cfg.SecretsKeysetFile)
	if err != nil {
		return err
	}

	authenticator, err := auth.New(ctx, auth.Options{
		IssuerURL:          cfg.OIDCIssuerURL,
		Audience:           cfg.OIDCAudience,
		EmailClaim:         cfg.OIDCEmailClaim,
		EmailVerifiedClaim: cfg.OIDCEmailVerifiedClaim,
		GroupsClaim:        cfg.OIDCGroupsClaim,
	})
	if err != nil {
		return err
	}

	queries := db.New(server.Database())
	api := secrets.NewAPI(secrets.NewBroker(queries, cipher), audit.NewRecorder(stdout, queries), logger)
	api.Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	return nil
}

// newLogger returns the JSON logger writing to out at level (already
// validated by config.Validate).
func newLogger(out io.Writer, level string) *slog.Logger {
	var slogLevel slog.Level

	_ = slogLevel.UnmarshalText([]byte(level))

	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slogLevel}))
}
