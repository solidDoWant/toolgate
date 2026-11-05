package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// FullPass returns the reconciler's full pass: it reads every instance row
// that isn't stopped.
func FullPass(queries *db.Queries, logger *slog.Logger) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		instances, err := queries.ListInstancesNotStopped(ctx)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		logger.DebugContext(ctx, "runtime reconciler pass", slog.Int("instances", len(instances)))

		return nil
	}
}
