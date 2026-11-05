package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
// replica holding the Lease. The returned channel receives the manager's
// result once ctx ends or the replica loses the Lease.
func startRuntime(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) (<-chan error, error) {
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

	if err := mgr.Add(&runtime.Reconciler{
		Pass:      runtime.FullPass(db.New(pool), logger),
		Subscribe: listener.Subscribe,
		Fence:     lease,
		Client:    mgr.GetClient(),
		Log:       logger,
	}); err != nil {
		return nil, fmt.Errorf("kubernetes manager: %w", err)
	}

	go func() { _ = listener.Run(ctx) }()

	result := make(chan error, 1)

	go func() { result <- mgr.Start(ctx) }()

	return result, nil
}
