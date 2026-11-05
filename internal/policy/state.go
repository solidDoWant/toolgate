package policy

import (
	"fmt"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
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
