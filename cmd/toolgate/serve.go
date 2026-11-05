package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
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

	if err := migrateDatabase(ctx, server.Database(), logger); err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	authenticator := newAuthenticator(ctx, cfg, logger)

	var broker *secrets.Broker

	if cfg.SecretsKeysetFile != "" {
		var err error

		broker, err = registerSecretsAPI(server, cfg, authenticator, stdout, logger)
		if err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}
	}

	templates := catalog.New()
	rules := policy.NewRules()

	// One notification listener serves the waiting enables and the runtime
	// reconciler.
	listener := store.NewListener(server.Database(), logger)

	server.Handle(gateway.MCPPath, authenticator.Middleware(gateway.NewMCPHandler(gateway.MCPOptions{
		Database:    server.Database(),
		Catalog:     templates,
		Policy:      &policy.Engine{Rules: rules},
		AuditOutput: stdout,
		Logger:      logger,
		Subscribe:   listener.Subscribe,

		InstanceLimitPerUser: cfg.InstanceLimitPerUser,
		InstanceLimit:        cfg.InstanceLimit,
		SharedNamespace:      cfg.SharedNamespace,
	}), true))

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

	go func() { _ = listener.Run(ctx) }()

	var runtimeResult chan error

	if cfg.LeaseNamespace != "" {
		managerDone, err := startRuntime(ctx, runtimeOptions{
			cfg:             cfg,
			pool:            server.Database(),
			catalog:         templates,
			rules:           rules,
			secrets:         userSecrets(broker),
			registerMetrics: server.RegisterMetrics,
			logger:          logger,
			listener:        listener,
		})
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

// migrateDatabase applies the schema migrations before the gateway opens its
// listeners or joins leader election (docs/design/phase-1.md §7). Replicas
// starting together take turns behind goose's PostgreSQL session lock. A
// database newer than this binary (a rollback) is only logged: migrations
// never run down.
func migrateDatabase(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("database migrations: %w", err)
	}

	database, binary, err := store.SchemaVersions(ctx, pool)
	if err != nil {
		return fmt.Errorf("database migrations: %w", err)
	}

	if database > binary {
		logger.Warn("the database schema is newer than this gateway; starting anyway",
			"databaseVersion", database, "gatewayVersion", binary)
	}

	return nil
}

// newAuthenticator returns the authenticator of the gateway's user-facing
// endpoints. It starts discovering the identity provider in the background;
// requests are refused until discovery succeeds.
func newAuthenticator(ctx context.Context, cfg config.Config, logger *slog.Logger) *lazyAuthenticator {
	authenticator := &lazyAuthenticator{
		ctx: context.WithoutCancel(ctx),
		options: auth.Options{
			IssuerURL:          cfg.OIDCIssuerURL,
			Audience:           cfg.OIDCAudience,
			EmailClaim:         cfg.OIDCEmailClaim,
			EmailVerifiedClaim: cfg.OIDCEmailVerifiedClaim,
			GroupsClaim:        cfg.OIDCGroupsClaim,
		},
		logger: logger,
	}

	go func() {
		if _, err := authenticator.get(); err != nil {
			logger.WarnContext(ctx, "cannot reach the identity provider yet", "error", err)
		}
	}()

	return authenticator
}

// registerSecretsAPI serves `/api/v1/secrets` behind user authentication
// and returns the secret broker.
func registerSecretsAPI(server *gateway.Server, cfg config.Config, authenticator *lazyAuthenticator, stdout io.Writer,
	logger *slog.Logger,
) (*secrets.Broker, error) {
	cipher, err := secrets.LoadKeyset(cfg.SecretsKeysetFile)
	if err != nil {
		return nil, err
	}

	queries := db.New(server.Database())
	broker := secrets.NewBroker(queries, cipher)
	api := secrets.NewAPI(broker, audit.NewRecorder(stdout, queries), logger)
	api.Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	return broker, nil
}

// errNoSecretsKeyset is the render error for `${secret:…}` on a gateway
// without a secrets keyset.
var errNoSecretsKeyset = errors.New("user secrets are not configured on this gateway")

// userSecrets returns the secret reader of instance renders: broker, or one
// that fails every read when the gateway has no secrets keyset.
func userSecrets(broker *secrets.Broker) runtime.SecretGetter {
	if broker == nil {
		return func(context.Context, string, string) ([]byte, time.Time, error) {
			return nil, time.Time{}, errNoSecretsKeyset
		}
	}

	return broker.Get
}

// newLogger returns the JSON logger writing to out at level (already
// validated by config.Validate).
func newLogger(out io.Writer, level string) *slog.Logger {
	var slogLevel slog.Level

	_ = slogLevel.UnmarshalText([]byte(level))

	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slogLevel}))
}
