package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// InstanceEntry is one live instance shown on the web UI's Servers page.
type InstanceEntry struct {
	ID string
	// Server is the template's name, Namespace its namespace.
	Server    string
	Namespace string
	Scope     string
	// Owner is the principal the instance runs for; empty when shared.
	Owner   string
	Status  string
	Reason  string
	ReadyAt *time.Time
	// Outdated says the instance was started from an older version of the
	// template, or from a template that is no longer offered.
	Outdated bool
}

// CatalogView answers the web UI's questions about the catalog and the
// instances with the same rules as the meta tools.
type CatalogView struct {
	tools *metaTools
}

// NewCatalogView returns the catalog view of database, templates and the
// policy engine.
func NewCatalogView(database *pgxpool.Pool, templates *catalog.Catalog, engine *policy.Engine) *CatalogView {
	return &CatalogView{tools: &metaTools{database: database, catalog: templates, policy: engine}}
}

// Servers returns the catalog as toolgate_list_servers shows it to
// identity, with nothing enabled.
func (v *CatalogView) Servers(ctx context.Context, identity auth.Identity) ([]ServerEntry, error) {
	state, err := v.tools.policyState(ctx, identity)
	if err != nil {
		return nil, err
	}

	current := requestContext{identity: identity}
	enabled := map[types.NamespacedName]bool{}
	servers := []ServerEntry{}

	for _, template := range v.tools.catalog.List() {
		entry, visible, err := v.tools.entry(ctx, current, template, enabled, state)
		if err != nil {
			return nil, err
		}

		if visible {
			servers = append(servers, entry)
		}
	}

	return servers, nil
}

// LiveInstances returns the live instances owned by principal and, with
// includeShared, the shared ones.
func (v *CatalogView) LiveInstances(ctx context.Context, principal string, includeShared bool) ([]InstanceEntry, error) {
	rows, err := db.New(v.tools.database).ListLiveInstancesForUI(ctx, db.ListLiveInstancesForUIParams{
		OwnerPrincipal: principal,
		IncludeShared:  includeShared,
	})
	if err != nil {
		return nil, fmt.Errorf("read live instances: %w", err)
	}

	instances := make([]InstanceEntry, 0, len(rows))

	for _, row := range rows {
		entry := InstanceEntry{
			ID:        row.ID,
			Server:    row.TemplateName,
			Namespace: row.TemplateNs,
			Scope:     row.Scope,
			Owner:     row.OwnerPrincipal,
		}

		entry.Status, entry.Reason = shownStatus(row)

		if row.ReadyAt.Valid {
			readyAt := row.ReadyAt.Time
			entry.ReadyAt = &readyAt
		}

		entry.Outdated = v.outdated(row)

		instances = append(instances, entry)
	}

	return instances, nil
}

// Errors of CatalogView.RequestRestart.
var (
	// ErrInstanceNotFound means the instance doesn't exist or isn't the
	// caller's to restart.
	ErrInstanceNotFound = errors.New("instance not found")
	// ErrInstanceChanged means the instance isn't live, ready and outdated
	// any more.
	ErrInstanceChanged = errors.New("instance changed")
)

// RequestRestart records a restart request for an instance from the web UI
// (docs/design/phase-2.md §6, "Restart", step 1). principal must own the
// instance; a shared instance needs admin. The instance must be live, ready
// and outdated. The request and the instance notification commit in one
// transaction; the reconciler then replaces the instance.
func (v *CatalogView) RequestRestart(ctx context.Context, id, principal string, admin bool) error {
	return pgx.BeginFunc(ctx, v.tools.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		row, err := queries.GetInstance(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInstanceNotFound
		}

		if err != nil {
			return fmt.Errorf("read instance: %w", err)
		}

		owned := row.OwnerPrincipal != "" && row.OwnerPrincipal == principal
		if row.Scope == catalog.ScopeShared {
			owned = admin
		}

		if !owned {
			return ErrInstanceNotFound
		}

		if row.Desired != "running" || row.Status != "ready" || !v.outdated(row) {
			return ErrInstanceChanged
		}

		changed, err := queries.RequestInstanceRestart(ctx, db.RequestInstanceRestartParams{
			ID: id, RequestedBy: pgtype.Text{String: principal, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("request restart: %w", err)
		}

		if changed == 0 {
			return ErrInstanceChanged
		}

		return store.Notify(ctx, tx, store.KindInstance, id)
	})
}

// outdated reports whether row was started from an older version of its
// template, or from a template that is no longer offered.
func (v *CatalogView) outdated(row db.Instance) bool {
	template, err := v.tools.catalog.Offered(row.TemplateName)
	if err != nil || template.Namespace != row.TemplateNs {
		return true
	}

	return row.Generation != template.Generation || row.TemplateUid != string(template.UID)
}
