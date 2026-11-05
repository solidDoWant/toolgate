package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Grant bounds (docs/design/phase-2.md §2 and §5, "Grant form").
const (
	MinGrantLifetime = time.Minute
	MaxGrantLifetime = 7 * 24 * time.Hour

	// grantListRows bounds the grants shown on the Grants page. A user has
	// at most 100 live grants, and the cleanup deletes the others 7 days
	// after they stop being live.
	grantListRows = 500
	// sessionListRows bounds the open sessions offered on the grant form.
	sessionListRows = 100
)

// Audit event types of grants.
const (
	TypeGrantCreate = "grant.create"
	TypeGrantRevoke = "grant.revoke"
)

// ErrGrantNotFound is returned for a grant that doesn't exist, belongs to
// another user, or (for a revoke) was already revoked.
var ErrGrantNotFound = errors.New("grant not found")

// GrantRefusal is a grant request the creation rules refuse. Its message
// is shown to the user.
type GrantRefusal struct{ Message string }

func (r *GrantRefusal) Error() string { return r.Message }

// GrantableServer is a server the user can create grants for.
type GrantableServer struct {
	Name string
	// WholeServer says a whole-server grant is allowed: the server's
	// environment or one of its tools' is opt-in.
	WholeServer bool
	// Tools are the tools a tool grant is allowed for: the server's own
	// environment is any and the tool's is opt-in.
	Tools []string
}

// GrantRequest is a grant a user asks for.
type GrantRequest struct {
	Server string
	// Tool is empty for a whole-server grant.
	Tool string
	// Session is empty, or the X-Toolgate-Session value the grant is
	// narrowed to.
	Session  string
	Lifetime time.Duration
}

// Grants creates, revokes and lists a user's grants for the web UI with the
// rules of docs/design/phase-2.md §2.
type Grants struct {
	database    *pgxpool.Pool
	catalog     *catalog.Catalog
	auditOutput io.Writer
}

// NewGrants returns the grants service. Audit events go to auditOutput and
// the audit_events table.
func NewGrants(database *pgxpool.Pool, templates *catalog.Catalog, auditOutput io.Writer) *Grants {
	return &Grants{database: database, catalog: templates, auditOutput: auditOutput}
}

// Servers returns the servers identity can create grants for, by name.
func (g *Grants) Servers(identity auth.Identity) []GrantableServer {
	servers := []GrantableServer{}

	for _, template := range g.catalog.List() {
		if server, ok := grantable(identity, template); ok {
			servers = append(servers, server)
		}
	}

	slices.SortFunc(servers, func(a, b GrantableServer) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})

	return servers
}

// grantable applies the creation rules to one template.
func grantable(identity auth.Identity, template *v1alpha1.MCPServerTemplate) (GrantableServer, bool) {
	if !policy.HasAccess(identity, template.Spec.Access) {
		return GrantableServer{}, false
	}

	server := GrantableServer{Name: template.Name, WholeServer: isOptIn(template.Spec.Access.Environments)}

	for name, override := range template.Spec.Tools {
		if override.Environments == "" || !isOptIn(override.Environments) {
			continue
		}

		server.WholeServer = true

		if template.Spec.Access.Environments == policy.EnvironmentAny {
			server.Tools = append(server.Tools, name)
		}
	}

	slices.Sort(server.Tools)

	return server, server.WholeServer
}

// isOptIn reports whether an environments value is opt-in. An empty value
// on the server is the CRD's default, opt-in.
func isOptIn(environment string) bool {
	return environment == "" || environment == policy.EnvironmentOptIn
}

// OpenSessions returns the X-Toolgate-Session values of principal's open
// sessions, most recently used first.
func (g *Grants) OpenSessions(ctx context.Context, principal string) ([]string, error) {
	rows, err := db.New(g.database).ListOpenSessionsOfPrincipal(ctx, db.ListOpenSessionsOfPrincipalParams{
		Principal: principal, MaxRows: sessionListRows,
	})
	if err != nil {
		return nil, fmt.Errorf("read open sessions: %w", err)
	}

	sessions := make([]string, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, row.SessionID)
	}

	return sessions, nil
}

// List returns principal's live grants and the recent ones that are no
// longer live, newest first.
func (g *Grants) List(ctx context.Context, principal string) ([]db.Grant, error) {
	grants, err := db.New(g.database).ListGrantsOfPrincipal(ctx, db.ListGrantsOfPrincipalParams{
		Principal: principal, MaxRows: grantListRows,
	})
	if err != nil {
		return nil, fmt.Errorf("read grants: %w", err)
	}

	return grants, nil
}

// Create checks request against the creation rules for identity, then
// inserts the grant, writes grant.create and notifies grant:<id> in one
// transaction. A refused request returns a *GrantRefusal.
func (g *Grants) Create(ctx context.Context, identity auth.Identity, request GrantRequest) (db.Grant, error) {
	if request.Lifetime < MinGrantLifetime || request.Lifetime > MaxGrantLifetime {
		return db.Grant{}, &GrantRefusal{Message: "A grant lasts from 1 minute to 7 days."}
	}

	template, err := g.catalog.Offered(request.Server)
	if err != nil {
		return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf("Server %q isn't in your catalog.", request.Server)}
	}

	server, ok := grantable(identity, template)
	if !ok {
		if !policy.HasAccess(identity, template.Spec.Access) {
			return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf("Server %q isn't in your catalog.", request.Server)}
		}

		return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf("Server %q needs no grant.", request.Server)}
	}

	if request.Tool == "" && !server.WholeServer {
		return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf("Server %q needs no grant.", request.Server)}
	}

	if request.Tool != "" && !slices.Contains(server.Tools, request.Tool) {
		return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf(
			"Tool %q can't have its own grant. Only an opt-in tool on a server that needs no grant can. Create a grant for the whole server instead.",
			request.Server+"__"+request.Tool)}
	}

	queries := db.New(g.database)

	var session pgtype.Text

	if request.Session != "" {
		row, err := queries.GetSession(ctx, db.GetSessionParams{Principal: identity.Principal, SessionID: request.Session})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.EndedAt.Valid) {
			return db.Grant{}, &GrantRefusal{Message: "That session isn't one of your open sessions."}
		}

		if err != nil {
			return db.Grant{}, fmt.Errorf("read session: %w", err)
		}

		session = pgtype.Text{String: request.Session, Valid: true}
	}

	generated, err := uuid.NewV7()
	if err != nil {
		return db.Grant{}, fmt.Errorf("grant ID: %w", err)
	}

	grant, err := store.InsertGrantThen(ctx, g.database, db.InsertGrantWithinLimitParams{
		ID:           hex.EncodeToString(generated[:]),
		Principal:    identity.Principal,
		TemplateNs:   template.Namespace,
		TemplateName: template.Name,
		Tool:         request.Tool,
		SessionID:    session,
		Lifetime:     pgtype.Interval{Microseconds: request.Lifetime.Microseconds(), Valid: true},
	}, func(tx pgx.Tx, grant db.Grant) error {
		return g.recordAndNotify(ctx, tx, TypeGrantCreate, identity.Principal, grant)
	})
	if errors.Is(err, store.ErrLimitReached) {
		return db.Grant{}, &GrantRefusal{Message: fmt.Sprintf(
			"You have %d live grants. Revoke some first.", store.MaxLiveGrantsPerPrincipal)}
	}

	return grant, err
}

// Revoke revokes principal's own live grant id, writes grant.revoke and
// notifies grant:<id> in one transaction. It returns ErrGrantNotFound for a
// grant that doesn't exist, belongs to someone else, or is already revoked.
func (g *Grants) Revoke(ctx context.Context, principal, id string) (db.Grant, error) {
	var grant db.Grant

	err := pgx.BeginFunc(ctx, g.database, func(tx pgx.Tx) error {
		revoked, err := db.New(tx).RevokeGrantOfPrincipal(ctx, db.RevokeGrantOfPrincipalParams{ID: id, Principal: pgtype.Text{String: principal, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGrantNotFound
		}

		if err != nil {
			return fmt.Errorf("revoke grant: %w", err)
		}

		grant = revoked

		return g.recordAndNotify(ctx, tx, TypeGrantRevoke, principal, revoked)
	})

	return grant, err
}

// Get returns principal's own grant id, or ErrGrantNotFound.
func (g *Grants) Get(ctx context.Context, principal, id string) (db.Grant, error) {
	grant, err := db.New(g.database).GetGrant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && grant.Principal != principal) {
		return db.Grant{}, ErrGrantNotFound
	}

	if err != nil {
		return db.Grant{}, fmt.Errorf("read grant: %w", err)
	}

	return grant, nil
}

func (g *Grants) recordAndNotify(ctx context.Context, tx pgx.Tx, eventType, principal string, grant db.Grant) error {
	details := map[string]any{
		"grant":      grant.ID,
		"namespace":  grant.TemplateNs,
		"expires_at": grant.ExpiresAt.Time.UTC().Format(time.RFC3339),
	}

	session := ""
	if grant.SessionID.Valid {
		session = grant.SessionID.String
	}

	event := audit.Event{
		Type:        eventType,
		Principal:   principal,
		Session:     session,
		Attestation: auth.AttestationUser,
		Server:      grant.TemplateName,
		Tool:        grant.Tool,
		Details:     details,
	}

	if err := audit.NewRecorder(g.auditOutput, db.New(tx)).Record(ctx, event); err != nil {
		return err
	}

	return store.Notify(ctx, tx, store.KindGrant, grant.ID)
}
