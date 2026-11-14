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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/resultcache"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
	"github.com/solidDoWant/toolgate/internal/testtime"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
	"github.com/solidDoWant/toolgate/internal/ui"
	"github.com/solidDoWant/toolgate/internal/volumesapi"
	"github.com/solidDoWant/toolgate/internal/wellknown"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
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

	for _, name := range cfg.IgnoredSettings {
		logger.Warn("ignoring a setting that was removed: sessions always list toolgate_enable_server and toolgate_disable_server", "setting", name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	server, err := gateway.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	if err := migrateDatabase(ctx, server.Database(), cfg.MigrateOnStart, logger); err != nil {
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

	tail := audittail.New(audittail.Options{
		Queries:    db.New(server.Database()),
		AdminGroup: cfg.AdminGroup,
		Log:        logger,
	})
	tail.Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})
	server.RegisterOnShutdown(tail.Shutdown)

	payloads, err := newAuditPayloads(ctx, cfg, server)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	searchMetrics := embeddings.NewMetrics()

	// With scale-to-zero on, every replica records when it needs embeddings
	// and when a session with tool search on starts or resumes, so that the
	// leader starts the embeddings server and keeps it running.
	var embeddingsDemand func(context.Context)

	var embeddingsScaledDown func(context.Context) bool

	if cfg.ToolSearchEmbeddingsScaleToZeroDeployment != "" {
		embeddingsDemand = (&runtime.EmbeddingsDemand{Pool: server.Database(), Log: logger}).Record
		embeddingsScaledDown = runtime.EmbeddingsServerScaledDown(server.Database())
	}

	searchEmbedder, err := newToolSearchEmbedder(cfg, searchMetrics, embeddingsDemand, embeddingsScaledDown)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	if searchEmbedder != nil {
		if err := server.RegisterMetrics(searchMetrics.Collectors()...); err != nil {
			logger.Error("gateway failed to start", "error", err)

			return 1
		}
	}

	templates := catalog.New()
	rules := policy.NewRules()

	// One notification listener serves the waiting enables and the runtime
	// reconciler.
	listener := store.NewListener(server.Database(), logger)

	engine := &policy.Engine{Rules: rules}
	ruleView := &ruleLister{}

	workspaces := gateway.NewWorkspaces(server.Database(), templates, stdout, time.Duration(cfg.ApprovalTimeout))
	workspaces.SetWorkspaceKeys(cfg.WorkspaceKeys, cfg.WorkspaceKeysPrefix)

	(&sessionsapi.Handler{
		Queries:     db.New(server.Database()),
		IdleTimeout: testtime.Scale(time.Duration(cfg.SessionIdleTimeout)),
		Entries:     workspaces,
		Log:         logger,
	}).Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	(&volumesapi.Handler{
		Queries:    db.New(server.Database()),
		Pool:       server.Database(),
		AdminGroup: cfg.AdminGroup,
		Output:     stdout,
		Log:        logger,
	}).Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	profiles := &lateReader{}

	(&settings.Handler{
		Reader:     profiles,
		Namespaces: cfg.WatchedNamespaces(),
		Log:        logger,
	}).Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	if cfg.UIEnabled() {
		// The built-in server is offered only with the web UI, where
		// widening a scope can take a step-up.
		templates.SetBuiltin(gateway.BuiltinTemplate())

		engine.GrantURL = ui.GrantLink(cfg.PublicURL)

		if err := registerUI(server, cfg, gateway.NewCatalogView(server.Database(), templates, engine), ruleView,
			gateway.NewGrants(server.Database(), templates, stdout), workspaces, broker, payloads, stdout, logger); err != nil {
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

	(&workspacesapi.Handler{
		Workspaces:  workspaces,
		Keys:        workspaces,
		Enabled:     cfg.UIEnabled(),
		ApprovalURL: approvalURL,
		Log:         logger,
	}).Register(server, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	metrics := runtime.NewMetrics()
	if err := server.RegisterMetrics(metrics.Collectors()...); err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	resultCache := resultcache.NewPostgres(server.Database(), cfg.ToolResultCacheMaxEntryBytes, int64(cfg.ToolResultCacheMaxTemplateBytes))

	exchanges, credentialsCipher, err := newTokenExchanges(cfg, server.RegisterMetrics, logger)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
	}

	mcpHandler := gateway.NewMCPHandler(gateway.MCPOptions{
		ResultCache:   resultCache,
		Metrics:       metrics,
		Database:      server.Database(),
		Catalog:       templates,
		Policy:        engine,
		AuditOutput:   stdout,
		AuditPayloads: payloads,
		Logger:        logger,
		Subscribe:     listener.Subscribe,

		SubscribeReplies: listener.SubscribeReplies,

		InstanceLimitPerUser: cfg.InstanceLimitPerUser,
		InstanceLimit:        cfg.InstanceLimit,
		SharedNamespace:      cfg.SharedNamespace,
		ApprovalURL:          approvalURL,
		ApprovalTimeout:      time.Duration(cfg.ApprovalTimeout),
		WorkspaceScoping:     cfg.UIEnabled(),
		Workspaces:           workspaces,
		SessionIdleTimeout:   testtime.Scale(time.Duration(cfg.SessionIdleTimeout)),
		ToolSearch:           cfg.ToolSearch,
		Embedder:             searchEmbedder,

		ToolSearchSessionStarted: embeddingsDemand,
		Secrets:                  runtime.BrokerSecrets(userSecrets(broker)),
		TokenExchanges:           exchanges.gatewaySource(),
		CredentialsCipher:        credentialsCipher,
	})
	server.Handle(gateway.MCPPath, authenticator.Middleware(mcpHandler, true))
	// The GET streams end when shutdown starts, so their clients reconnect
	// to another replica while requests in progress finish.
	server.RegisterOnShutdown(mcpHandler.Shutdown)

	if err := registerOTLPIngest(server, cfg, authenticator, logger); err != nil {
		logger.Error("gateway failed to start", "error", err)

		return 1
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
			exchanges:       exchanges,
			profiles:        profiles,
			secrets:         userSecrets(broker),
			registerMetrics: server.RegisterMetrics,
			metrics:         metrics,
			logger:          logger,
			auditOutput:     stdout,
			listener:        listener,
			tracerProvider:  server.TracerProvider(),
			embedder:        searchEmbedder,
			resultCache:     resultCache,
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
// listeners or joins leader election. Replicas
// starting together take turns behind goose's PostgreSQL session lock. A
// database newer than this binary (a rollback) is only logged: migrations
// never run down.
func migrateDatabase(ctx context.Context, pool *pgxpool.Pool, migrateOnStart bool, logger *slog.Logger) error {
	var (
		database, binary int64
		err              error
	)

	if migrateOnStart {
		if err := store.Migrate(ctx, pool); err != nil {
			return fmt.Errorf("database migrations: %w", err)
		}

		database, binary, err = store.SchemaVersions(ctx, pool)
	} else {
		// With migrateOnStart false the gateway never changes the schema:
		// it refuses to start on an older one.
		database, binary, err = store.CheckSchemaVersion(ctx, pool)
		if err == nil {
			err = store.EnsureAuditPartitions(ctx, pool)
		}
	}

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
// their service-account tokens, checked by TokenReview. When
// workspaceAttestation is set, a user's request may carry a workload token
// that verifies its workspace.
func newAuthenticator(ctx context.Context, cfg config.Config, server *gateway.Server, stdout io.Writer,
	logger *slog.Logger,
) (*lazyAuthenticator, error) {
	var (
		serviceAccounts *auth.ServiceAccountOptions
		attestation     = cfg.AttestationOptions()
	)

	if len(cfg.ServiceAccountNamespaces) > 0 || attestation != nil {
		restConfig, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes: %w", err)
		}

		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: %w", err)
		}

		if len(cfg.ServiceAccountNamespaces) > 0 {
			serviceAccounts = &auth.ServiceAccountOptions{Client: clientset, Namespaces: cfg.ServiceAccountNamespaces}
		}

		if attestation != nil {
			attestation.Client = clientset
		}
	}

	authenticator := &lazyAuthenticator{
		ctx: context.WithoutCancel(ctx),
		options: auth.Options{
			IssuerURL:          cfg.OIDCIssuerURL,
			Audiences:          cfg.OIDCAudience,
			AdditionalIssuers:  cfg.AdditionalIssuers(),
			EmailClaim:         cfg.OIDCEmailClaim,
			EmailVerifiedClaim: cfg.OIDCEmailVerifiedClaim,
			GroupsClaim:        cfg.OIDCGroupsClaim,
			Recorder: refusalRecorder{
				recorder: audit.NewRecorder(stdout, db.New(server.Database())),
				logger:   logger,
			},
			ServiceAccounts: serviceAccounts,
			SPIFFE:          spiffeOptions(cfg, logger),
			Attestation:     attestation,
			WorkspaceKeys:   workspaceKeyOptions(cfg, server),
			Logger:          logger,
			// The MCP endpoint's 401 names the protected resource
			// metadata that gateway.New serves.
			ResourceMetadataURL: wellknown.ProtectedResourceMetadataURL(cfg.MCPResourceURL()),
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

// registerOTLPIngest serves the OTLP ingest routes behind authentication when
// otlpIngest is on. Without it the routes are not registered, so they answer
// 404.
func registerOTLPIngest(server *gateway.Server, cfg config.Config, authenticator *lazyAuthenticator,
	logger *slog.Logger,
) error {
	if !cfg.OTLPIngest {
		return nil
	}

	otlp := ingest.New(ingest.Options{
		Endpoints: map[string]string{
			ingest.SignalMetrics: cfg.OTLPEndpointFor("metrics"),
			ingest.SignalLogs:    cfg.OTLPEndpointFor("logs"),
			ingest.SignalTraces:  cfg.OTLPEndpointFor("traces"),
		},
		DropSessionIDFromMetrics: cfg.TelemetryDropSessionIDFromMetrics,
		AllowContent:             cfg.TelemetryAllowContent,
		Logger:                   logger,
	})
	otlp.Register(server.Handle, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	return server.RegisterMetrics(otlp.Collectors()...)
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
func registerUI(server *gateway.Server, cfg config.Config, servers ui.ServerSource, rules ui.RuleSource, grants ui.GrantSource, workspaces ui.WorkspaceSource, broker *secrets.Broker, payloads *audit.Payloads, stdout io.Writer, logger *slog.Logger) error {
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
		Grants:              grants,
		Workspaces:          workspaces,
		Logger:              logger,
		Servers:             servers,
		Pool:                server.Database(),
		Rules:               rules,
		ApprovalTimeout:     time.Duration(cfg.ApprovalTimeout),
		SessionResumeWindow: testtime.Scale(time.Duration(cfg.SessionResumeWindow)),
		AuditPayloads:       cfg.AuditPayloads,
		AuditOutput:         stdout,
	}

	// A nil *audit.Payloads must not become a non-nil interface.
	if payloads != nil {
		options.Payloads = payloads
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

// lateReader reads from the manager's informer cache once the manager exists.
// Before that, and on a gateway that runs no manager, it lists nothing.
type lateReader struct {
	source atomic.Pointer[client.Reader]
}

func (r *lateReader) set(reader client.Reader) { r.source.Store(&reader) }

func (r *lateReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	source := r.source.Load()
	if source == nil {
		return apierrors.NewNotFound(schema.GroupResource{}, key.Name)
	}

	return (*source).Get(ctx, key, obj, opts...)
}

func (r *lateReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	source := r.source.Load()
	if source == nil {
		return nil
	}

	return (*source).List(ctx, list, opts...)
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

// spiffeOptions turns the spiffeTrustDomains setting into the
// authenticator's SPIFFE options, or nil when it is empty.
func spiffeOptions(cfg config.Config, logger *slog.Logger) *auth.SPIFFEOptions {
	if len(cfg.SPIFFETrustDomains) == 0 {
		return nil
	}

	domains := make([]auth.SPIFFETrustDomain, 0, len(cfg.SPIFFETrustDomains))
	for _, domain := range cfg.SPIFFETrustDomains {
		domains = append(domains, auth.SPIFFETrustDomain{
			Name:              domain.Name,
			BundleEndpointURL: domain.BundleEndpointURL,
			JWKSURL:           domain.JWKSURL,
			CAFile:            domain.CAFile,
		})
	}

	return &auth.SPIFFEOptions{TrustDomains: domains, Logger: logger}
}

// newTokenExchanges returns the source of the exchangers of TokenExchange
// resources, and the cipher that seals their tokens for the leader. Without
// a secrets keyset it returns neither: every TokenExchange is then invalid,
// so no template uses one.
func newTokenExchanges(cfg config.Config, registerMetrics func(...prometheus.Collector) error, logger *slog.Logger,
) (*lateExchangeSource, *secrets.Cipher, error) {
	if cfg.SecretsKeysetFile == "" {
		return nil, nil, nil
	}

	cipher, err := secrets.LoadKeyset(cfg.SecretsKeysetFile)
	if err != nil {
		return nil, nil, err
	}

	metrics := credentials.NewMetrics()
	if err := registerMetrics(metrics.Collectors()...); err != nil {
		return nil, nil, err
	}

	transport, err := credentials.NewTransport(cfg.TokenExchangeCAFile)
	if err != nil {
		return nil, nil, err
	}

	return &lateExchangeSource{
		allowList: tokenExchangeAllowList(cfg),
		transport: transport,
		metrics:   metrics,
		logger:    logger,
	}, cipher, nil
}

// tokenExchangeAllowList returns the token endpoints exchanges may use: the
// chart's list when it sets one, or else the token endpoints of the user
// OIDC issuers.
func tokenExchangeAllowList(cfg config.Config) tokenexchange.AllowList {
	if len(cfg.TokenExchangeAllowedTokenURLs) > 0 {
		return tokenexchange.StaticAllowList(cfg.TokenExchangeAllowedTokenURLs)
	}

	return &tokenexchange.DiscoveredAllowList{Issuers: cfg.UserIssuerURLs()}
}

// lateExchangeSource builds exchangers from the TokenExchange resources in
// the manager's cache. It builds none until the manager exists.
type lateExchangeSource struct {
	allowList tokenexchange.AllowList
	transport http.RoundTripper
	metrics   *credentials.Metrics
	logger    *slog.Logger

	source atomic.Pointer[tokenexchange.Source]
}

// set gives the source the manager's cache and the template secrets
// reader. secretSource is nil when the gateway has no template secrets
// namespace.
func (l *lateExchangeSource) set(reader client.Reader, secretSource tokenexchange.SecretSource) {
	l.source.Store(&tokenexchange.Source{
		Reader: reader, AllowList: l.allowList, Secrets: secretSource,
		Transport: l.transport, Metrics: l.metrics, Logger: l.logger,
	})
}

// gatewaySource returns l as the gateway's exchange source, or nil when
// the gateway runs no exchanges.
func (l *lateExchangeSource) gatewaySource() gateway.ExchangeSource {
	if l == nil {
		return nil
	}

	return l
}

// Exchanger implements gateway.ExchangeSource.
func (l *lateExchangeSource) Exchanger(ctx context.Context, namespace, name string) (*credentials.Exchanger, error) {
	source := l.source.Load()
	if source == nil {
		return nil, fmt.Errorf("token exchange `%s`: the gateway hasn't read the TokenExchange resources yet", name)
	}

	return source.Exchanger(ctx, namespace, name)
}
