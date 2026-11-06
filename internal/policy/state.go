package policy

import (
	"fmt"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
)

// State is the policy state one request loads from PostgreSQL before it
// decides (docs/design/phase-2.md §3, "Loading grants and blocks"). Every
// decision of that request uses the same State, so Decide stays pure.
type State struct {
	// Grants are the principal's live grants. They are empty for attested
	// identities, whose decisions grants never change.
	Grants []Grant
	// Blocks are the live temporary blocks, oldest first.
	Blocks []*TemporaryBlock
	// Scope is the effective list of a signed-in user's session. Its zero
	// value doesn't apply, so it allows every server.
	Scope Scope
}

// Scope is the effective list of servers of a signed-in user's request
// (docs/design/phase-3.md §10, "The effective list"). It only refuses: a
// server it allows still has to pass every other step.
type Scope struct {
	// Applies is set for identities of kind user on a gateway with the web
	// UI. When it is false, step 4 passes every server.
	Applies bool
	// All allows every server.
	All bool
	// Servers are the allowed server names when All is false.
	Servers map[string]bool
}

// allows reports whether the scope lets identity use server. Only
// identities of kind user are ever scoped.
func (s Scope) allows(identity auth.Identity, server string) bool {
	if !s.Applies || identity.Kind != auth.KindUser {
		return true
	}

	return s.All || s.Servers[server]
}

// Grant is one live grant: it lets Principal pass an opt-in environment on
// one server, or on one tool of it.
type Grant struct {
	ID                string
	Principal         string
	TemplateNamespace string
	TemplateName      string
	// Tool is the tool's own name, or empty for a whole-server grant.
	Tool string
	// Session is the X-Toolgate-Session value the grant is narrowed to, or
	// empty for every session.
	Session string
}

// TemporaryBlock is one live temporary block, with its match compiled.
// Blocks are never changed after insert, so a compiled block can be reused
// for as long as its row is live.
type TemporaryBlock struct {
	ID        string
	Reason    string
	ExpiresAt time.Time

	match *compiledRule
}

// NewTemporaryBlock compiles a temporary block's match with the code that
// compiles ToolgateRule matches, so a block matches exactly as a rule with
// the same match would.
func NewTemporaryBlock(id string, match v1alpha1.RuleMatch, reason string, expiresAt time.Time) (*TemporaryBlock, error) {
	compiled := &compiledRule{}
	if err := compileMatch(match, compiled); err != nil {
		return nil, fmt.Errorf("temporary block %s: %w", id, err)
	}

	return &TemporaryBlock{ID: id, Reason: reason, ExpiresAt: expiresAt, match: compiled}, nil
}

// refusal is the reason a temporary block gives (docs/design/phase-2.md §3).
func (b *TemporaryBlock) refusal() string {
	return fmt.Sprintf("Blocked by an admin until %s: %s (block %s).", b.ExpiresAt.UTC().Format(time.RFC3339), b.Reason, b.ID)
}

// grantFor returns the first grant in state that lets identity's principal
// pass step 2 for this request: same principal and server, an empty tool
// or this tool (whole-server requests count only empty ones), and no
// session or this request's session.
func (s State) grantFor(principal, session, namespace, server, tool string) (Grant, bool) {
	for _, grant := range s.Grants {
		if grant.Principal != principal || grant.TemplateNamespace != namespace || grant.TemplateName != server {
			continue
		}

		if grant.Tool != "" && grant.Tool != tool {
			continue
		}

		if grant.Session != "" && grant.Session != session {
			continue
		}

		return grant, true
	}

	return Grant{}, false
}

// Values of a user's scoping defaults.
const (
	ScopeDefaultNone = "none"
	ScopeDefaultAll  = "all"
)

// ScopeRows are the scoping rows of one signed-in user's request, as
// stored.
type ScopeRows struct {
	// VerifiedDefault and UnverifiedDefault are the user_scopes row's
	// defaults. Empty, as for a missing row, means none.
	VerifiedDefault   string
	UnverifiedDefault string
	// HasList says the user has a list for the request's verified
	// workspace; ListServers are its servers.
	HasList     bool
	ListServers []string
	// Additions are the servers added to the request's session only.
	Additions []string
}

// EffectiveScope builds the scope of a request from its rows
// (docs/design/phase-3.md §10, "The effective list"). In a verified
// workspace it is the workspace's list if there is one, else the verified
// default, plus the session's additions. An unverified session gets the
// unverified default and nothing more: a claimed workspace never selects a
// list and a claimed session ID never adds servers. Identities that aren't
// of kind user get a scope that doesn't apply.
func EffectiveScope(identity auth.Identity, rows ScopeRows) Scope {
	if identity.Kind != auth.KindUser {
		return Scope{}
	}

	if identity.Attestation != attestationAttested {
		return Scope{Applies: true, All: rows.UnverifiedDefault == ScopeDefaultAll}
	}

	scope := Scope{Applies: true, Servers: make(map[string]bool, len(rows.ListServers)+len(rows.Additions))}

	if rows.HasList {
		for _, server := range rows.ListServers {
			scope.Servers[server] = true
		}
	} else {
		scope.All = rows.VerifiedDefault == ScopeDefaultAll
	}

	for _, server := range rows.Additions {
		scope.Servers[server] = true
	}

	return scope
}
