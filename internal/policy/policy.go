// Package policy decides whether an identity may list or enable a server and
// call its tools (SPEC.md §10). Phase 1 has no grants, approvals or temporary
// blocks: a decision checks catalog access, then the environment, then the
// blocks (a tool's blocked flag and the standing ToolgateRule objects).
package policy

import (
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

	serviceAccountPrefix = "system:serviceaccount:"
)

// Request is one question to the policy engine.
type Request struct {
	Identity auth.Identity
	// Template is the server's catalog entry. Its name is the server name.
	Template *v1alpha1.MCPServerTemplate
	// Tool is the tool's own name, without the server prefix. It is empty
	// for list_servers and enable_server, which decide for the whole server.
	Tool string
}

// Decision is the outcome of a Request.
type Decision struct {
	Allowed bool
	// Step is the step that refused. It is empty when Allowed.
	Step string
	// Rule is the namespace/name of the ToolgateRule that refused, when
	// Step is StepRule.
	Rule string
	// Reason is a short sentence for the model and the audit log.
	Reason string
}

// Engine decides requests against the standing rules.
type Engine struct {
	// Rules are the standing ToolgateRule objects. Nil means none.
	Rules *Rules
	// Now returns the current time, used for rule expiry. Nil means
	// time.Now.
	Now func() time.Time
}

// Decide applies SPEC.md §10 in order: catalog access, environment, blocks.
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

	if !environmentAllows(environment, identity.Attestation) {
		subject := fmt.Sprintf("Server %q", server)
		if request.Tool != "" {
			subject = fmt.Sprintf("Tool %q", proxiedName(server, request.Tool))
		}

		return refuse(StepEnvironment, "", subject+" can be used only from an attested environment, not from a signed-in user's machine.")
	}

	if override.Blocked {
		return refuse(StepBlocked, "", fmt.Sprintf("Tool %q is blocked by the server's template.", proxiedName(server, request.Tool)))
	}

	if e.Rules != nil {
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
		}

		if rule, found := e.Rules.match(subject, e.now()); found {
			name := rule.key.String()

			return refuse(StepRule, name, fmt.Sprintf("Refused by rule %s: %s", name, rule.reason))
		}
	}

	return Decision{Allowed: true}
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
// first step of a decision.
// Service accounts match only serviceAccounts, and users only users and
// groups, so an IdP subject that looks like a service account name can't
// pass as one.
func HasAccess(identity auth.Identity, access v1alpha1.Access) bool {
	if isServiceAccount(identity) {
		return slices.Contains(access.ServiceAccounts, identity.Principal)
	}

	if slices.Contains(access.Users, identity.Principal) {
		return true
	}

	if identity.Email != "" && slices.Contains(access.Users, identity.Email) {
		return true
	}

	return slices.ContainsFunc(identity.Groups, func(group string) bool {
		return slices.Contains(access.Groups, group)
	})
}

// isServiceAccount reports whether the identity is a Kubernetes service
// account. Only workload tokens verified by TokenReview are attested with
// such a principal.
func isServiceAccount(identity auth.Identity) bool {
	return identity.Attestation == attestationAttested && strings.HasPrefix(identity.Principal, serviceAccountPrefix)
}

// environmentAllows reports whether an attestation level satisfies an
// environments value. An empty value is opt-in, the CRD's default. In phase 1
// there are no grants, so opt-in needs attested.
func environmentAllows(environment, attestation string) bool {
	switch environment {
	case EnvironmentAny:
		return attestation == attestationUser || attestation == attestationAttested
	default:
		// attested, and opt-in (also when empty) without grants.
		return attestation == attestationAttested
	}
}
