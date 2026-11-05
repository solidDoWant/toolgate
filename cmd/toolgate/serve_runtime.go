package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// startRuntime starts the PostgreSQL notification listener and the
// controller-runtime manager that runs the runtime reconciler on the
// replica holding the Lease, and registers the runtime metrics with
// registerMetrics. The returned channel receives the manager's
// result once ctx ends or the replica loses the Lease.
func startRuntime(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, registerMetrics func(...prometheus.Collector) error,
	logger *slog.Logger,
) (<-chan error, error) {
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

	options := runtime.ManagerOptions(lease, timing)
	options.Logger = kubeLogger
	options.Metrics = metricsserver.Options{BindAddress: "0"}

	mgr, err := manager.New(restConfig, options)
	if err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	listener := store.NewListener(pool, logger)

	reconciler := &runtime.Reconciler{
		Subscribe: listener.Subscribe,
		Fence:     lease,
		Client:    mgr.GetClient(),
	}

	metrics := runtime.NewMetrics()
	if err := registerMetrics(metrics.Collectors()...); err != nil {
		return nil, err
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
		Pool:    pool,
		Reader:  mgr.GetAPIReader(),
		Deleter: reconciler,
		Log:     logger,
	}

	// This process doesn't read the template catalog yet, and judging
	// instances against an empty catalog would stop them all, so it makes
	// no stop decisions and sends no instance back to pending. Failed
	// instances are still torn down.
	queries := db.New(pool)
	reconciler.Pass = sweep.Pass(teardown.Pass(queries, runtime.FullPass(queries, logger, teardown.Step)))

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	go func() { _ = listener.Run(ctx) }()

	result := make(chan error, 1)

	go func() { result <- mgr.Start(ctx) }()

	return result, nil
}
