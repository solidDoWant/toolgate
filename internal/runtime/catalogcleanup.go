package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// CatalogCleaner deletes tool catalog rows that are no longer needed: those
// of templates that no longer exist, and, for each template, every row older
// than the newest two generations (docs/design/phase-4.md §2, "Cleanup").
// Only the leader runs it, on its full passes.
type CatalogCleaner struct {
	Queries *db.Queries
	// TemplateUIDs returns the UIDs of the templates that exist.
	TemplateUIDs func() map[string]bool
}

// Pass wraps a full pass so that stale catalog rows are deleted before it.
func (c *CatalogCleaner) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := c.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

// Run deletes the stale rows.
func (c *CatalogCleaner) Run(ctx context.Context) error {
	stored, err := c.Queries.ListToolCatalogUIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tool catalog templates: %w", err)
	}

	existing := c.TemplateUIDs()

	var gone []string

	for _, uid := range stored {
		if !existing[uid] {
			gone = append(gone, uid)
		}
	}

	var problems []error

	if len(gone) > 0 {
		if _, err := c.Queries.DeleteToolCatalogOfTemplates(ctx, gone); err != nil {
			problems = append(problems, fmt.Errorf("delete tool catalog of removed templates: %w", err))
		}
	}

	if err := c.deleteSearchRows(ctx, existing); err != nil {
		problems = append(problems, err)
	}

	if _, err := c.Queries.DeleteToolCatalogOlderThanNewestTwo(ctx); err != nil {
		problems = append(problems, fmt.Errorf("delete old tool catalog generations: %w", err))
	}

	return errors.Join(problems...)
}

// deleteSearchRows deletes the tool search rows of templates that no longer
// exist.
func (c *CatalogCleaner) deleteSearchRows(ctx context.Context, existing map[string]bool) error {
	stored, err := c.Queries.ListToolSearchUIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tool search templates: %w", err)
	}

	var gone []string

	for _, uid := range stored {
		if !existing[uid] {
			gone = append(gone, uid)
		}
	}

	if len(gone) == 0 {
		return nil
	}

	if _, err := c.Queries.DeleteToolSearchOfTemplates(ctx, gone); err != nil {
		return fmt.Errorf("delete tool search of removed templates: %w", err)
	}

	return nil
}
