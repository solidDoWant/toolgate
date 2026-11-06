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
// (docs/design/phase-3.md §10, "The effective list"), with the scope
// entries that apply to it (docs/design/phase-4.md §7, "Step 4 with scope
// entries"). It only refuses: a server it allows still has to pass every
// other step.
type Scope struct {
	// Applies is set for identities of kind user on a gateway with the web
	// UI. When it is false, step 4 passes every server.
	Applies bool
	// All allows every server.
	All bool
	// Servers are the allowed server names when All is false.
	Servers map[string]bool
	// Denied maps a server to the deny entries that name it, by upstream
	// tool name. The empty tool denies the whole server.
	Denied map[string]map[string]DeniedIn
	// AllowedTools maps a server to the upstream names of the tools that
	// an allow entry names.
	AllowedTools map[string]map[string]bool
}

// DeniedIn says where a deny entry was made.
type DeniedIn string

// Places of a deny entry.
const (
	DeniedInWorkspace DeniedIn = "workspace"
	DeniedInSession   DeniedIn = "session"
)

// scopeOutcome is step 4's answer for one server, or one tool of it.
type scopeOutcome struct {
	allowed bool
	// denied is set when a deny entry refused.
	denied DeniedIn
}

// decide applies step 4 to identity's request for server and tool (empty
// for a decision about the whole server). Only identities of kind user are
// ever scoped.
func (s Scope) decide(identity auth.Identity, server, tool string) scopeOutcome {
	if !s.Applies || identity.Kind != auth.KindUser {
		return scopeOutcome{allowed: true}
	}

	// Rule 1: a deny of the whole server, or of this tool.
	denies := s.Denied[server]
	if where, found := denies[""]; found {
		return scopeOutcome{denied: where}
	}

	if tool != "" {
		if where, found := denies[tool]; found {
			return scopeOutcome{denied: where}
		}
	}

	// Rule 2: the effective list.
	if s.All || s.Servers[server] {
		return scopeOutcome{allowed: true}
	}

	// Rule 3: an allow entry of this tool, or, for the whole server, of any
	// tool of it that isn't denied.
	allowed := s.AllowedTools[server]
	if tool != "" {
		return scopeOutcome{allowed: allowed[tool]}
	}

	for name := range allowed {
		if _, found := denies[name]; !found {
			return scopeOutcome{allowed: true}
		}
	}

	return scopeOutcome{}
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

// Effects of a scope entry.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// ScopeEntry is one stored scope entry that may apply to a request.
type ScopeEntry struct {
	// Session is set for an entry of the request's session, and false for
	// one of its workspace.
	Session bool
	Server  string
	// Tool is an upstream tool name, or empty for the whole server.
	Tool   string
	Effect string
}

// ScopeRows are the scoping rows of one signed-in user's request, as
// stored.
type ScopeRows struct {
	// VerifiedDefault and UnverifiedDefault are the user_scopes row's
	// defaults. Empty, as for a missing row, means none.
	VerifiedDefault   string
	UnverifiedDefault string
	// UnverifiedList is the user_scopes row's unverified_list: with
	// UnverifiedDefault none, the default for unverified sessions is list,
	// and UnverifiedServers are its servers (docs/design/phase-4.md §2).
	UnverifiedList    bool
	UnverifiedServers []string
	// HasList says the user has a list for the request's verified
	// workspace; ListServers are its servers.
	HasList     bool
	ListServers []string
	// Additions are the servers added to the request's session only.
	Additions []string
	// Entries are the scope entries of the request's verified workspace
	// and of its session.
	Entries []ScopeEntry
}

// EffectiveScope builds the scope of a request from its rows
// (docs/design/phase-3.md §10, "The effective list", and
// docs/design/phase-4.md §7). In a verified workspace it is the workspace's
// list if there is one, else the verified default, plus the session's
// additions, and the workspace's and the session's entries apply. An
// unverified session gets the unverified default, or its allow list, and
// only the session's deny entries: a claimed workspace never selects a list
// or entries, and a claimed session ID never adds servers. Identities that
// aren't of kind user get a scope that doesn't apply.
func EffectiveScope(identity auth.Identity, rows ScopeRows) Scope {
	if identity.Kind != auth.KindUser {
		return Scope{}
	}

	if identity.Attestation != attestationAttested {
		scope := Scope{Applies: true, All: rows.UnverifiedDefault == ScopeDefaultAll}
		if !scope.All && rows.UnverifiedList {
			scope.Servers = make(map[string]bool, len(rows.UnverifiedServers))
			for _, server := range rows.UnverifiedServers {
				scope.Servers[server] = true
			}
		}

		for _, entry := range rows.Entries {
			if entry.Session && entry.Effect == EffectDeny {
				scope.addEntry(entry)
			}
		}

		return scope
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

	for _, entry := range rows.Entries {
		scope.addEntry(entry)
	}

	return scope
}

// addEntry adds one entry to the scope's tool sets. A workspace deny wins
// over a session deny of the same name, so the refusal names the
// workspace.
func (s *Scope) addEntry(entry ScopeEntry) {
	switch entry.Effect {
	case EffectDeny:
		if s.Denied == nil {
			s.Denied = map[string]map[string]DeniedIn{}
		}

		if s.Denied[entry.Server] == nil {
			s.Denied[entry.Server] = map[string]DeniedIn{}
		}

		where := DeniedInWorkspace
		if entry.Session {
			where = DeniedInSession
		}

		if current, found := s.Denied[entry.Server][entry.Tool]; !found || current == DeniedInSession {
			s.Denied[entry.Server][entry.Tool] = where
		}
	case EffectAllow:
		if entry.Tool == "" {
			// Server-level allows are lists and additions, not entries.
			return
		}

		if s.AllowedTools == nil {
			s.AllowedTools = map[string]map[string]bool{}
		}

		if s.AllowedTools[entry.Server] == nil {
			s.AllowedTools[entry.Server] = map[string]bool{}
		}

		s.AllowedTools[entry.Server][entry.Tool] = true
	}
}
