package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// runtimeOptions are the dependencies of startRuntime.
type runtimeOptions struct {
	cfg  config.Config
	pool *pgxpool.Pool
	// catalog and rules are filled by their reconcilers, on every replica.
	catalog *catalog.Catalog
	rules   *policy.Rules
	// ruleLister gets the manager's cache for the Blocks page. Nil is fine.
	ruleLister *ruleLister
	// secrets reads the users' secrets for instance renders.
	secrets         runtime.SecretGetter
	registerMetrics func(...prometheus.Collector) error
	logger          *slog.Logger
	// auditOutput receives the audit lines of the reconciler's writes.
	auditOutput io.Writer
	// listener delivers the PostgreSQL notifications. serve runs it.
	listener *store.Listener
	// tracerProvider makes the render and apply spans.
	tracerProvider trace.TracerProvider
}

// skipControllerNameValidation turns off controller-runtime's check that
// controller names are unique in the process. Only tests set it: they run
// serve more than once in one process. A gateway process runs serve once.
var skipControllerNameValidation bool

// startRuntime starts the controller-runtime manager. On every replica the manager runs the catalog
// and rule reconcilers, which write status only on the leader. On the
// replica holding the Lease it runs the runtime reconciler and the health
// monitor. It registers the runtime metrics with registerMetrics. The
// returned channel receives the manager's result once ctx ends or the
// replica loses the Lease.
func startRuntime(ctx context.Context, options runtimeOptions) (<-chan error, error) {
	cfg, pool, logger := options.cfg, options.pool, options.logger

	kubeLogger := logr.FromSlogHandler(logger.Handler())
	ctrl.SetLogger(kubeLogger)

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("leader election identity: %w", err)
	}

	timing := runtime.DefaultElectionTiming

	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, cfg.LeaseNamespace, runtime.LeaseName,
		clientset.CoreV1(), clientset.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: hostname + "_" + uuid.NewString()})
	if err != nil {
		return nil, fmt.Errorf("leader election: %w", err)
	}

	lease := runtime.NewLease(lock, timing.RenewDeadline)

	watched := cfg.WatchedNamespaces()

	managerOptions := runtime.ManagerOptions(lease, timing)
	managerOptions.Logger = kubeLogger
	managerOptions.Metrics = metricsserver.Options{BindAddress: "0"}
	managerOptions.Scheme = kubeScheme()
	// The informers for the catalog read only the watched namespaces.
	managerOptions.Cache = cache.Options{ByObject: map[client.Object]cache.ByObject{
		&v1alpha1.MCPServerTemplate{}: {Namespaces: catalog.CacheNamespaces(watched)},
		&v1alpha1.ToolgateRule{}:      {Namespaces: catalog.CacheNamespaces(watched)},
	}}

	if skipControllerNameValidation {
		managerOptions.Controller.SkipNameValidation = ptr.To(true)
	}

	mgr, err := manager.New(restConfig, managerOptions)
	if err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	listener := options.listener

	reconciler := &runtime.Reconciler{
		Subscribe: listener.Subscribe,
		Fence:     lease,
		Client:    mgr.GetClient(),
	}

	resolverConfigs := make([]runtime.ResolverConfig, 0, len(cfg.Resolvers))
	for _, resolver := range cfg.Resolvers {
		resolverConfigs = append(resolverConfigs, runtime.ResolverConfig{
			Name: resolver.Name, URL: resolver.URL, Timeout: resolver.ResolverTimeout(), CAFile: resolver.CAFile,
		})
	}

	resolvers, err := runtime.NewHTTPResolvers(resolverConfigs)
	if err != nil {
		return nil, err
	}

	validator := catalog.Validator{SharedNamespace: cfg.SharedNamespace, Resolvers: resolvers.Names()}

	if options.ruleLister != nil {
		options.ruleLister.set(mgr.GetClient(), watched)
	}

	if err := setupCatalog(mgr, options.catalog, options.rules, watched, validator, logger); err != nil {
		return nil, err
	}

	metrics := runtime.NewMetrics()
	if err := options.registerMetrics(metrics.Collectors()...); err != nil {
		return nil, err
	}

	// The Lease holder creates the audit partitions when it gets the Lease
	// and every 24 h, so a long-running gateway never reaches a month
	// without one.
	partitions := store.NewAuditPartitionKeeper(pool, logger)
	if err := options.registerMetrics(partitions.Errors()); err != nil {
		return nil, err
	}

	if err := mgr.Add(partitions); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	teardown := &runtime.Teardown{
		Pool:    pool,
		Reader:  mgr.GetClient(),
		Deleter: reconciler,
		Log:     logger,
		Metrics: metrics,
	}

	// The sweep deletes namespaces that belong to no live instance on every
	// pass, starting with the pass on gaining the Lease.
	sweep := &runtime.Sweep{
		Pool:            pool,
		Reader:          mgr.GetAPIReader(),
		Deleter:         reconciler,
		Templates:       options.catalog,
		SharedNamespace: cfg.SharedNamespace,
		Log:             logger,
	}

	// Idle sessions end before the instances are judged, so their
	// per-session instances stop in the same pass.
	sessionEnder := &runtime.SessionEnder{
		Pool:         pool,
		IdleTimeout:  testtime.Scale(time.Duration(cfg.SessionIdleTimeout)),
		ResumeWindow: testtime.Scale(time.Duration(cfg.SessionResumeWindow)),
		AuditOutput:  options.auditOutput,
		Log:          logger,
	}

	approvalExpirer := &runtime.ApprovalExpirer{Pool: pool, AuditOutput: options.auditOutput}

	stops := &runtime.StopDecider{Pool: pool, Templates: options.catalog, Log: logger}
	replacer := &runtime.Replacer{Pool: pool, Stops: stops, AuditOutput: options.auditOutput, Log: logger}

	startStore := runtime.PostgresStartStore{
		Pool: pool, AuditOutput: options.auditOutput,
		DeclaresTools: func(instance db.Instance) bool {
			template, found := options.catalog.Running(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})

			return found && string(template.UID) == instance.TemplateUid && len(template.Spec.DeclaredTools) > 0
		},
		RunningTemplate: func(instance db.Instance) (*v1alpha1.MCPServerTemplate, bool) {
			template, found := options.catalog.Running(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})

			return template, found && string(template.UID) == instance.TemplateUid
		},
	}

	starter := &runtime.Starter{
		Store:            startStore,
		Client:           mgr.GetClient(),
		Template:         options.catalog.Running,
		Secrets:          runtime.BrokerSecrets(options.secrets),
		Resolvers:        resolvers,
		ListTools:        runtime.MCPToolLister,
		GatewayNamespace: cfg.LeaseNamespace,
		GatewayPodLabels: cfg.PodLabels(),
		TracerProvider:   options.tracerProvider,
		Logger:           logger,
	}

	repairer := &runtime.Repairer{
		Pool:        pool,
		Replacer:    replacer,
		Starter:     starter,
		AuditOutput: options.auditOutput,
		Log:         logger,
	}

	// The monitor runs only on the Lease holder, like every runnable that
	// does not say otherwise.
	monitor := &runtime.Monitor{
		Store:    startStore,
		Template: options.catalog.Running,
		Connect:  runtime.MCPMonitorConnector,
		Logger:   logger,
	}
	if err := mgr.Add(manager.RunnableFunc(monitor.Run)); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	// Judging instances against a partial catalog would stop instances whose
	// template exists, so the first pass waits until every watched template
	// has been read.
	queries := db.New(pool)
	catalogCleaner := &runtime.CatalogCleaner{Queries: queries, TemplateUIDs: options.catalog.UIDs}
	reconciler.Pass = runtime.AfterLoaded(options.catalog.Loaded(),
		runtime.RowCleanup(queries, catalogCleaner.Pass(approvalExpirer.Pass(sweep.Pass(sessionEnder.Pass(teardown.Pass(queries,
			runtime.FullPass(queries, logger, stops.Step, replacer.Step, replacer.ExpiryStep, repairer.Step, teardown.Step, starter.Step, monitor.Step))))))))

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	// The search rebuilder needs leader election, so it syncs every template
	// each time this replica gets the Lease.
	searchRows := &runtime.SearchRebuilder{
		Pool: pool, Fence: lease, Templates: options.catalog.LastValidByUID,
		Changes: options.catalog.Subscribe, Loaded: options.catalog.Loaded(), Log: logger,
	}
	if err := mgr.Add(manager.RunnableFunc(searchRows.Run)); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	result := make(chan error, 1)

	go func() { result <- mgr.Start(ctx) }()

	return result, nil
}

// setupCatalog registers the catalog and rule reconcilers, which run on
// every replica and write status only while it is leader, and the loader
// that marks the catalog loaded.
func setupCatalog(mgr manager.Manager, templates *catalog.Catalog, rules *policy.Rules, watched []string, validator catalog.Validator, logger *slog.Logger) error {
	catalogReconciler := &catalog.Reconciler{
		Client:     mgr.GetClient(),
		Catalog:    templates,
		Validator:  validator,
		Log:        logger,
		Elected:    mgr.Elected(),
		Namespaces: watched,
	}
	if err := catalogReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("kubernetes manager: %w", err)
	}

	ruleReconciler := &policy.RuleReconciler{
		Client:     mgr.GetClient(),
		Rules:      rules,
		Log:        logger,
		Elected:    mgr.Elected(),
		Namespaces: watched,
	}
	if err := ruleReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("kubernetes manager: %w", err)
	}

	if err := mgr.Add(&catalog.Loader{Reader: mgr.GetCache(), Catalog: templates, Namespaces: watched}); err != nil {
		return fmt.Errorf("kubernetes manager: %w", err)
	}

	return nil
}

// kubeScheme returns the client-go types and toolgate's custom resources.
func kubeScheme() *k8sruntime.Scheme {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return scheme
}
