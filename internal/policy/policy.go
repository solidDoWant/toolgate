// Package policy decides whether an identity may list or enable a server and
// call its tools (SPEC.md §10, docs/design/phase-2.md §3). A decision checks
// catalog access, then the environment (where a live grant passes opt-in for
// a signed-in user), then the blocks: a tool's blocked flag, the standing
// ToolgateRule objects and the live temporary blocks.
package policy

import (
	"github.com/gobwas/glob"

	"fmt"
	"slices"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
)

// Steps of a decision. A refusal names the step that refused.
const (
	StepAccess      = "access"
	StepEnvironment = "environment"
	StepBlocked     = "blocked"
	StepRule        = "rule"
	// StepTemporaryBlock is a live temporary block made by an admin.
	StepTemporaryBlock = "temporary-block"
	// StepWorkspaceScope is a server outside the effective list of a
	// signed-in user's session (docs/design/phase-3.md §10).
	StepWorkspaceScope = "workspace-scope"
)

// Values of access.environments and its per-tool override.
const (
	EnvironmentAny      = "any"
	EnvironmentOptIn    = "opt-in"
	EnvironmentAttested = "attested"
)

const (
	attestationUser     = auth.AttestationUser
	attestationAttested = auth.AttestationAttested
)

// Request is one question to the policy engine.
type Request struct {
	Identity auth.Identity
	// Template is the server's catalog entry. Its name is the server name.
	Template *v1alpha1.MCPServerTemplate
	// Tool is the tool's own name, without the server prefix. It is empty
	// for list_servers and enable_server, which decide for the whole server.
	Tool string
	// State is the request's live grants and temporary blocks. The zero
	// value has none.
	State State
}

// Decision is the outcome of a Request.
type Decision struct {
	Allowed bool
	// Step is the step that refused. It is empty when Allowed.
	Step string
	// Rule is the namespace/name of the ToolgateRule that refused, when
	// Step is StepRule.
	Rule string
	// Grant is the ID of the grant that passed the environment step, if a
	// grant was needed.
	Grant string
	// Block is the ID of the temporary block that refused, when Step is
	// StepTemporaryBlock.
	Block string
	// Reason is a short sentence for the model and the audit log.
	Reason string
	// NeedsApproval is set on an allow of a tool with `approval: required`:
	// the call goes on only with a person's approval
	// (docs/design/phase-2.md §4).
	NeedsApproval bool
}

// Engine decides requests against the standing rules.
type Engine struct {
	// Rules are the standing ToolgateRule objects. Nil means none.
	Rules *Rules
	// Now returns the current time, used for rule expiry. Nil means
	// time.Now.
	Now func() time.Time
	// GrantURL returns the web UI link that creates a grant for server,
	// tool (empty for the whole server) and session. Nil means the gateway
	// has no web UI: a missing grant then gets the plain environment
	// refusal.
	GrantURL func(server, tool, session string) string
}

// Decide applies SPEC.md §10 in order: catalog access, environment, blocks,
// workspace scope, approval.
// It reads the grants and temporary blocks from request.State only.
func (e *Engine) Decide(request Request) Decision {
	identity := request.Identity
	template := request.Template
	server := template.Name

	if !HasAccess(identity, template.Spec.Access) {
		return refuse(StepAccess, "", fmt.Sprintf("You don't have access to server %q.", server))
	}

	var override v1alpha1.ToolOverride
	if request.Tool != "" {
		override = template.Spec.Tools[request.Tool]
	}

	environment := template.Spec.Access.Environments
	if override.Environments != "" {
		environment = override.Environments
	}

	var grantID string

	if !environmentAllows(environment, identity.Attestation) {
		grant, granted := Grant{}, false
		if needsGrant(environment, identity.Attestation) {
			grant, granted = request.State.grantFor(identity.Principal, identity.Session, template.Namespace, server, request.Tool)
		}

		if !granted {
			return e.refuseEnvironment(request, environment)
		}

		grantID = grant.ID
	}

	if override.Blocked {
		return refuse(StepBlocked, "", fmt.Sprintf("Tool %q is blocked by the server's template.", proxiedName(server, request.Tool)))
	}

	if e.Rules != nil {
		subject := ruleTarget(identity, server, request.Tool)

		if rule, found := e.Rules.match(subject, e.now()); found {
			name := rule.key.String()

			return refuse(StepRule, name, fmt.Sprintf("Refused by rule %s: %s", name, rule.reason))
		}
	}

	if len(request.State.Blocks) > 0 {
		subject := ruleTarget(identity, server, request.Tool)
		for _, block := range request.State.Blocks {
			if block.match.applies(subject, time.Time{}) {
				return Decision{Step: StepTemporaryBlock, Block: block.ID, Grant: grantID, Reason: block.refusal()}
			}
		}
	}

	if !request.State.Scope.allows(identity, server) {
		return Decision{Step: StepWorkspaceScope, Grant: grantID, Reason: WorkspaceScopeRefusal(identity, server)}
	}

	return Decision{Allowed: true, Grant: grantID, NeedsApproval: override.Approval == ApprovalRequired}
}

// WorkspaceScopeRefusal is the reason of a step-4 refusal: in a verified
// workspace it names the workspace's list, in an unverified session the
// default for unverified sessions (docs/design/phase-3.md §10).
func WorkspaceScopeRefusal(identity auth.Identity, server string) string {
	if identity.Attestation == attestationAttested {
		return fmt.Sprintf("Server `%s` isn't in the list for workspace `%s`. Allow it in the Workspaces page or with `toolgate workspace allow %s`.", server, identity.Workspace, server)
	}

	return fmt.Sprintf("Server `%s` isn't allowed in sessions without a verified workspace. To allow all your servers in them, set the default for unverified sessions to all on the Workspaces page, or run `toolgate workspace defaults --unverified all`.", server)
}

// ApprovalRequired is the ToolOverride.Approval value that makes each call
// of the tool need a person's approval.
const ApprovalRequired = "required"

// refuseEnvironment is the refusal of step 2. When a grant would pass it
// and the gateway has a web UI, the reason links to the page that creates
// one (docs/design/phase-2.md §4, "A missing grant").
func (e *Engine) refuseEnvironment(request Request, environment string) Decision {
	server := request.Template.Name

	subject := fmt.Sprintf("Server %q", server)
	if request.Tool != "" {
		subject = fmt.Sprintf("Tool %q", proxiedName(server, request.Tool))
	}

	if e.GrantURL != nil && needsGrant(environment, request.Identity.Attestation) {
		subject = fmt.Sprintf("Server `%s`", server)
		if request.Tool != "" {
			subject = fmt.Sprintf("Tool `%s`", proxiedName(server, request.Tool))
		}

		link := e.GrantURL(server, request.Tool, request.Identity.Session)

		return refuse(StepEnvironment, "", fmt.Sprintf("%s needs a grant on a signed-in user's machine. Create one at %s, then try again.", subject, link))
	}

	return refuse(StepEnvironment, "", subject+" can be used only from an attested environment, not from a signed-in user's machine.")
}

// ruleTarget is what rules and temporary blocks are matched against.
func ruleTarget(identity auth.Identity, server, tool string) target {
	subject := target{
		principal:   identity.Principal,
		email:       identity.Email,
		groups:      identity.Groups,
		workspace:   identity.Workspace,
		session:     identity.Session,
		attestation: identity.Attestation,
		server:      server,
	}
	if tool != "" {
		subject.tool = proxiedName(server, tool)
	}

	return subject
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}

	return e.Now()
}

func refuse(step, rule, reason string) Decision {
	return Decision{Step: step, Rule: rule, Reason: reason}
}

func proxiedName(server, tool string) string {
	return server + "__" + tool
}

// HasAccess reports whether the identity is named by the access lists, the
// first step of a decision. The identity's kind alone picks the list:
// SPIFFE workloads match only spiffeIDs, service accounts only
// serviceAccounts, and users only users and groups, so a user whose subject
// looks like a SPIFFE ID or a service account name can't pass as one. An
// identity without a kind matches nothing.
func HasAccess(identity auth.Identity, access v1alpha1.Access) bool {
	switch identity.Kind {
	case auth.KindSPIFFE:
		return slices.ContainsFunc(access.SPIFFEIDs, func(pattern string) bool {
			compiled, err := glob.Compile(pattern)

			return err == nil && compiled.Match(identity.Principal)
		})
	case auth.KindServiceAccount:
		return slices.Contains(access.ServiceAccounts, identity.Principal)
	case auth.KindUser:
		if slices.Contains(access.Users, identity.Principal) {
			return true
		}

		if identity.Email != "" && slices.Contains(access.Users, identity.Email) {
			return true
		}

		return slices.ContainsFunc(identity.Groups, func(group string) bool {
			return slices.Contains(access.Groups, group)
		})
	default:
		return false
	}
}

// environmentAllows reports whether an attestation level satisfies an
// environments value without a grant. An empty value is opt-in, the CRD's
// default.
func environmentAllows(environment, attestation string) bool {
	switch environment {
	case EnvironmentAny:
		return attestation == attestationUser || attestation == attestationAttested
	default:
		// attested, and opt-in (also when empty) without a grant.
		return attestation == attestationAttested
	}
}

// needsGrant reports whether a live grant passes an environment that
// refused: opt-in (also when empty) for a signed-in user. Grants never
// count for attested.
func needsGrant(environment, attestation string) bool {
	return attestation == attestationUser && environment != EnvironmentAny && environment != EnvironmentAttested
}
