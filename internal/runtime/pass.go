package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Step moves one instance row forward. Starter.Step is one.
type Step func(ctx context.Context, instance db.Instance) error

// FullPass returns the reconciler's full pass: it reads every instance row
// that isn't stopped and runs each step on each row. An error on one row
// doesn't keep the other rows from being handled; the pass then returns the
// errors, so it is retried.
func FullPass(queries *db.Queries, logger *slog.Logger, steps ...Step) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		instances, err := queries.ListInstancesNotStopped(ctx)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		logger.DebugContext(ctx, "runtime reconciler pass", slog.Int("instances", len(instances)))

		var problems []error

		for _, instance := range instances {
			for _, step := range steps {
				if err := step(ctx, instance); err != nil {
					problems = append(problems, err)
				}
			}
		}

		return errors.Join(problems...)
	}
}

// AfterLoaded wraps a full pass so that it waits until loaded is closed
// (catalog.Catalog.Loaded): the first pass starts only after every watched
// template has been read. It returns ctx's error if ctx ends first.
func AfterLoaded(loaded <-chan struct{}, pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-loaded:
		case <-ctx.Done():
			return fmt.Errorf("waiting for the template catalog: %w", ctx.Err())
		}

		return pass(ctx)
	}
}
