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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
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
	"github.com/solidDoWant/toolgate/internal/ui"
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

	authenticator, err := newAuthenticator(ctx, cfg, server, stdout, logger)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	var broker *secrets.Broker

	if cfg.SecretsKeysetFile != "" {
		var err error

		broker, err = registerSecretsAPI(server, cfg, authenticator, stdout, logger)
		if err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}
	}

	payloads, err := newAuditPayloads(ctx, cfg, server)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	templates := catalog.New()
	rules := policy.NewRules()

	// One notification listener serves the waiting enables and the runtime
	// reconciler.
	listener := store.NewListener(server.Database(), logger)

	engine := &policy.Engine{Rules: rules}
	ruleView := &ruleLister{}

	if cfg.UIEnabled() {
		if err := registerUI(server, cfg, gateway.NewCatalogView(server.Database(), templates, engine), ruleView, broker, stdout, logger); err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}
	}

	var approvalURL func(id string) string
	if cfg.UIEnabled() {
		approvalURL = func(id string) string {
			return strings.TrimSuffix(cfg.PublicURL, "/") + "/ui/approvals/" + id
		}
	}

	server.Handle(gateway.MCPPath, authenticator.Middleware(gateway.NewMCPHandler(gateway.MCPOptions{
		Database:      server.Database(),
		Catalog:       templates,
		Policy:        engine,
		AuditOutput:   stdout,
		AuditPayloads: payloads,
		Logger:        logger,
		Subscribe:     listener.Subscribe,

		InstanceLimitPerUser: cfg.InstanceLimitPerUser,
		InstanceLimit:        cfg.InstanceLimit,
		SharedNamespace:      cfg.SharedNamespace,
		ApprovalURL:          approvalURL,
		ApprovalTimeout:      time.Duration(cfg.ApprovalTimeout),
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

	if err := startAuditRetention(ctx, server, payloads, logger); err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	var runtimeResult chan error

	if cfg.LeaseNamespace != "" {
		managerDone, err := startRuntime(ctx, runtimeOptions{
			cfg:             cfg,
			pool:            server.Database(),
			catalog:         templates,
			rules:           rules,
			ruleLister:      ruleView,
			secrets:         userSecrets(broker),
			registerMetrics: server.RegisterMetrics,
			logger:          logger,
			auditOutput:     stdout,
			listener:        listener,
			tracerProvider:  server.TracerProvider(),
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
// requests are refused until discovery succeeds. Failed sign-ins go to the
// audit log, and the count of those over its rate limit to /metrics. When
// serviceAccountNamespaces is set, pods in those namespaces sign in with
// their service-account tokens, checked by TokenReview.
func newAuthenticator(ctx context.Context, cfg config.Config, server *gateway.Server, stdout io.Writer,
	logger *slog.Logger,
) (*lazyAuthenticator, error) {
	var serviceAccounts *auth.ServiceAccountOptions

	if len(cfg.ServiceAccountNamespaces) > 0 {
		restConfig, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes: %w", err)
		}

		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: %w", err)
		}

		serviceAccounts = &auth.ServiceAccountOptions{Client: clientset, Namespaces: cfg.ServiceAccountNamespaces}
	}

	authenticator := &lazyAuthenticator{
		ctx: context.WithoutCancel(ctx),
		options: auth.Options{
			IssuerURL:          cfg.OIDCIssuerURL,
			Audience:           cfg.OIDCAudience,
			EmailClaim:         cfg.OIDCEmailClaim,
			EmailVerifiedClaim: cfg.OIDCEmailVerifiedClaim,
			GroupsClaim:        cfg.OIDCGroupsClaim,
			Recorder: refusalRecorder{
				recorder: audit.NewRecorder(stdout, db.New(server.Database())),
				logger:   logger,
			},
			ServiceAccounts: serviceAccounts,
		},
		logger:          logger,
		registerMetrics: server.RegisterMetrics,
	}

	go func() {
		if _, err := authenticator.get(); err != nil {
			logger.WarnContext(ctx, "cannot reach the identity provider yet", "error", err)
		}
	}()

	return authenticator, nil
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

// maxClientSecretBytes bounds the web UI client secret file.
const maxClientSecretBytes = 4 << 10

// registerUI serves the web UI under `/ui/` and `GET /`. It reads the UI
// client's secret, when one is configured, before the gateway starts.
func registerUI(server *gateway.Server, cfg config.Config, servers ui.ServerSource, rules ui.RuleSource, broker *secrets.Broker, stdout io.Writer, logger *slog.Logger) error {
	var clientSecret string

	if cfg.OIDCUIClientSecretFile != "" {
		secret, err := readSmallFile(cfg.OIDCUIClientSecretFile, maxClientSecretBytes)
		if err != nil {
			return fmt.Errorf("oidcUIClientSecretFile: %w", err)
		}

		clientSecret = strings.TrimSpace(string(secret))
		if clientSecret == "" {
			return errors.New("oidcUIClientSecretFile: the file is empty")
		}
	}

	options := ui.Options{
		Database:           server.Database(),
		IssuerURL:          cfg.OIDCIssuerURL,
		ClientID:           cfg.OIDCUIClientID,
		ClientSecret:       clientSecret,
		Scopes:             cfg.OIDCUIScopes,
		PublicURL:          cfg.PublicURL,
		AdminGroup:         cfg.AdminGroup,
		EmailClaim:         cfg.OIDCEmailClaim,
		EmailVerifiedClaim: cfg.OIDCEmailVerifiedClaim,
		GroupsClaim:        cfg.OIDCGroupsClaim,
		Recorder: refusalRecorder{
			recorder: audit.NewRecorder(stdout, db.New(server.Database())),
			logger:   logger,
		},
		Logger:          logger,
		Servers:         servers,
		Pool:            server.Database(),
		Rules:           rules,
		ApprovalTimeout: time.Duration(cfg.ApprovalTimeout),
		AuditPayloads:   cfg.AuditPayloads,
		AuditOutput:     stdout,
	}

	// A gateway without a secrets keyset has no Secrets page.
	if broker != nil {
		options.Secrets = broker
		options.Auditor = audit.NewRecorder(stdout, db.New(server.Database()))
	}

	webUI := ui.New(options, server)

	return server.RegisterMetrics(webUI.DroppedRefusals())
}

// readSmallFile reads a file of at most limit bytes.
func readSmallFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}

	return data, nil
}

// ruleLister lists the ToolgateRule objects from the manager's informer
// cache for the Blocks page. It lists none until the manager exists.
type ruleLister struct {
	source atomic.Pointer[ruleSource]
}

type ruleSource struct {
	reader     client.Reader
	namespaces []string
}

func (l *ruleLister) set(reader client.Reader, namespaces []string) {
	l.source.Store(&ruleSource{reader: reader, namespaces: namespaces})
}

// Rules returns the rules of the watched namespaces.
func (l *ruleLister) Rules(ctx context.Context) ([]v1alpha1.ToolgateRule, error) {
	source := l.source.Load()
	if source == nil {
		return nil, nil
	}

	var rules []v1alpha1.ToolgateRule

	for _, namespace := range source.namespaces {
		var list v1alpha1.ToolgateRuleList
		if err := source.reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("list rules in %s: %w", namespace, err)
		}

		rules = append(rules, list.Items...)
	}

	return rules, nil
}
