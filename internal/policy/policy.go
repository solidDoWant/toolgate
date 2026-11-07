// Package policy decides whether an identity may list or enable a server and
// call its tools. A decision checks
// catalog access, then the environment (where a live grant passes opt-in for
// a signed-in user), then the blocks: a tool's blocked flag, the standing
// ToolgateRule objects and the live temporary blocks.
package policy

import (
	"github.com/gobwas/glob"

	"fmt"
	"slices"
	"strings"
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
	// signed-in user's session.
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
	// ExposedTool is the name the tool is offered under, when it differs
	// from Tool. Rules match both names; every other step uses Tool.
	ExposedTool string
	// ProxiedTool is the name a session sees for the tool, when it isn't
	// <server>__<ExposedTool>: the built-in server's tools are offered
	// under exact names such as toolgate_allow. Rules and temporary blocks
	// match it too; every other step ignores it.
	ProxiedTool string
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
	// the call goes on only with a person's approval.
	NeedsApproval bool
	// Denied is set when a deny entry refused at step 4, and says where it
	// was made. Such a refusal is never asked about.
	Denied DeniedIn
}

// ReasonDenied is the audit reason of a step-4 refusal by a deny entry.
const ReasonDenied = "denied"

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

// Decide applies the checks in order: catalog access, environment, blocks,
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
		subject := ruleTarget(identity, server, request)

		if rule, found := e.Rules.match(subject, e.now()); found {
			name := rule.key.String()

			return refuse(StepRule, name, fmt.Sprintf("Refused by rule %s: %s", name, rule.reason))
		}
	}

	if len(request.State.Blocks) > 0 {
		subject := ruleTarget(identity, server, request)
		for _, block := range request.State.Blocks {
			if block.match.applies(subject, time.Time{}) {
				return Decision{Step: StepTemporaryBlock, Block: block.ID, Grant: grantID, Reason: block.refusal()}
			}
		}
	}

	scope := request.State.Scope.decide(identity, server, request.Tool)
	if scope.denied != "" {
		return Decision{Step: StepWorkspaceScope, Grant: grantID, Denied: scope.denied, Reason: deniedRefusal(identity, server, request.Tool, scope.denied)}
	}

	if !scope.allowed {
		return Decision{Step: StepWorkspaceScope, Grant: grantID, Reason: WorkspaceScopeRefusal(identity, server)}
	}

	return Decision{Allowed: true, Grant: grantID, NeedsApproval: override.Approval == ApprovalRequired}
}

// WorkspaceScopeRefusal is the reason of a step-4 refusal: in a verified
// workspace it names the workspace's list, in an unverified session the
// default for unverified sessions.
func WorkspaceScopeRefusal(identity auth.Identity, server string) string {
	if identity.Attestation == attestationAttested {
		return fmt.Sprintf("Server `%s` isn't in the list for workspace `%s`. Allow it in the Workspaces page or with `toolgate workspace allow %s`.", server, identity.Workspace, server)
	}

	return fmt.Sprintf("Server `%s` isn't allowed in sessions without a verified workspace. To allow all your servers in them, set the default for unverified sessions to all on the Workspaces page, or run `toolgate workspace defaults --unverified all`.", server)
}

// deniedRefusal is the reason of a step-4 refusal by a deny entry: it names
// the tool, or the server for a decision about the whole server, and where
// the entry was made.
func deniedRefusal(identity auth.Identity, server, tool string, where DeniedIn) string {
	subject := server
	if tool != "" {
		subject = proxiedName(server, tool)
	}

	if where == DeniedInWorkspace {
		return fmt.Sprintf("`%s` is denied in workspace `%s`.", subject, identity.Workspace)
	}

	return fmt.Sprintf("`%s` is denied in this session.", subject)
}

// ApprovalRequired is the ToolOverride.Approval value that makes each call
// of the tool need a person's approval.
const ApprovalRequired = "required"

// refuseEnvironment is the refusal of step 2. When a grant would pass it
// and the gateway has a web UI, the reason links to the page that creates
// one.
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
func ruleTarget(identity auth.Identity, server string, request Request) target {
	subject := target{
		principal:   identity.Principal,
		email:       identity.Email,
		groups:      identity.Groups,
		workspace:   identity.Workspace,
		session:     identity.Session,
		attestation: identity.Attestation,
		server:      server,
	}
	if request.Tool != "" {
		subject.tool = proxiedName(server, request.Tool)
		if request.ExposedTool != "" && request.ExposedTool != request.Tool {
			subject.exposedTool = proxiedName(server, request.ExposedTool)
		}

		subject.sessionTool = request.ProxiedTool
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
// identity without a kind matches nothing. Entries of spiffeIDs, users and
// groups are glob patterns matched against the whole value; a user's email
// is matched only when the identity provider verified it, because
// Identity.Email is empty otherwise.
func HasAccess(identity auth.Identity, access v1alpha1.Access) bool {
	switch identity.Kind {
	case auth.KindSPIFFE:
		return matchesAnyPattern(access.SPIFFEIDs, identity.Principal)
	case auth.KindServiceAccount:
		return slices.Contains(access.ServiceAccounts, identity.Principal)
	case auth.KindUser:
		if access.AllUsers || matchesAnyPattern(access.Users, identity.Principal) {
			return true
		}

		if identity.Email != "" && matchesAnyPattern(access.Users, identity.Email) {
			return true
		}

		return slices.ContainsFunc(identity.Groups, func(group string) bool {
			return matchesAnyPattern(access.Groups, group)
		})
	default:
		return false
	}
}

// globMetacharacters are the characters that make an access entry a
// pattern. An entry without any of them is compared as is, so it matches
// exactly what it did before entries became patterns, and costs no compile.
const globMetacharacters = `*?[]{}\`

// IsAccessPattern reports whether an access entry is a glob pattern rather
// than an exact value.
func IsAccessPattern(entry string) bool {
	return strings.ContainsAny(entry, globMetacharacters)
}

// matchesAnyPattern reports whether value matches one of the access entries.
// An invalid pattern matches nothing; the catalog marks its template invalid.
func matchesAnyPattern(entries []string, value string) bool {
	return slices.ContainsFunc(entries, func(entry string) bool {
		if !IsAccessPattern(entry) {
			return entry == value
		}

		compiled, err := glob.Compile(entry)

		return err == nil && compiled.Match(value)
	})
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
