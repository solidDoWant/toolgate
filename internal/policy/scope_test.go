package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func verifiedUser() auth.Identity {
	identity := laptopUser()
	identity.Attestation = auth.AttestationAttested
	identity.Workspace = "coder/alice/mail"

	return identity
}

func TestEffectiveScope(t *testing.T) {
	tests := []struct {
		name     string
		identity auth.Identity
		rows     policy.ScopeRows
		allowed  []string
		refused  []string
	}{
		{
			name:     "verified workspace with a list allows only the list",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", HasList: true, ListServers: []string{"mail"}},
			allowed:  []string{"mail"},
			refused:  []string{"k8s"},
		},
		{
			name:     "verified workspace with an empty list allows nothing",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", HasList: true},
			refused:  []string{"mail", "k8s"},
		},
		{
			name:     "verified workspace without a list and default all allows everything",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all"},
			allowed:  []string{"mail", "k8s"},
		},
		{
			name:     "verified workspace without a list and default none allows nothing",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "none", UnverifiedDefault: "all"},
			refused:  []string{"mail"},
		},
		{
			name:     "verified workspace adds the session's additions to the list",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{HasList: true, ListServers: []string{"mail"}, Additions: []string{"immich"}},
			allowed:  []string{"mail", "immich"},
			refused:  []string{"k8s"},
		},
		{
			name:     "verified workspace adds the session's additions to default none",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{Additions: []string{"immich"}},
			allowed:  []string{"immich"},
			refused:  []string{"mail"},
		},
		{
			name:     "missing user_scopes row means both defaults none",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{},
			refused:  []string{"mail"},
		},
		{
			name:     "unverified session with default all allows everything",
			identity: laptopUser(),
			rows:     policy.ScopeRows{UnverifiedDefault: "all"},
			allowed:  []string{"mail", "k8s"},
		},
		{
			name:     "unverified session with default none allows nothing",
			identity: laptopUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", UnverifiedDefault: "none"},
			refused:  []string{"mail"},
		},
		{
			name:     "unverified session never gets a list or additions",
			identity: laptopUser(),
			rows:     policy.ScopeRows{HasList: true, ListServers: []string{"mail"}, Additions: []string{"immich"}},
			refused:  []string{"mail", "immich"},
		},
		{
			name:     "missing row refuses an unverified session",
			identity: laptopUser(),
			refused:  []string{"mail"},
		},
		{
			name:     "service accounts are never scoped",
			identity: pod("ci"),
			allowed:  []string{"mail"},
		},
		{
			name:     "SPIFFE workloads are never scoped",
			identity: spiffeWorkload(),
			allowed:  []string{"mail"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := policy.State{Scope: policy.EffectiveScope(test.identity, test.rows)}
			decisions := engine(t)

			for _, server := range test.allowed {
				decision := decisions.Decide(policy.Request{Identity: test.identity, Template: template(server, anyone(), nil), State: state})
				assert.True(t, decision.Allowed, "server %s: %+v", server, decision)
			}

			for _, server := range test.refused {
				decision := decisions.Decide(policy.Request{Identity: test.identity, Template: template(server, anyone(), nil), State: state})
				assert.False(t, decision.Allowed, "server %s", server)
				assert.Equal(t, policy.StepWorkspaceScope, decision.Step, "server %s", server)
			}
		})
	}
}

func anyone() v1alpha1.Access {
	return v1alpha1.Access{
		Users:           []string{"alice-sub"},
		ServiceAccounts: []string{"system:serviceaccount:ci:runner"},
		SPIFFEIDs:       []string{"spiffe://coder.example.com/owner/alice/workspace/dev"},
		Environments:    policy.EnvironmentAny,
	}
}

func TestWorkspaceScopeStepOrder(t *testing.T) {
	nothing := policy.State{Scope: policy.Scope{Applies: true}}

	t.Run("a scope that doesn't apply allows every server", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{Identity: laptopUser(), Template: template("mail", anyone(), nil)})
		assert.True(t, decision.Allowed)
	})

	t.Run("catalog access refuses before the scope, which never widens", func(t *testing.T) {
		everything := policy.State{Scope: policy.Scope{Applies: true, All: true}}
		decision := engine(t).Decide(policy.Request{Identity: laptopUser(), Template: template("mail", v1alpha1.Access{Users: []string{"bob@example.com"}}, nil), State: everything})
		assert.Equal(t, policy.StepAccess, decision.Step)
	})

	t.Run("environment refuses before the scope", func(t *testing.T) {
		access := anyone()
		access.Environments = policy.EnvironmentAttested
		decision := engine(t).Decide(policy.Request{Identity: laptopUser(), Template: template("mail", access, nil), State: nothing})
		assert.Equal(t, policy.StepEnvironment, decision.Step)
	})

	t.Run("a blocked tool refuses before the scope", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{
			Identity: laptopUser(), Template: template("mail", anyone(), map[string]v1alpha1.ToolOverride{"send": {Blocked: true}}),
			Tool: "send", State: nothing,
		})
		assert.Equal(t, policy.StepBlocked, decision.Step)
	})

	t.Run("a rule refuses before the scope", func(t *testing.T) {
		decisions := engine(t, rule("no-mail", v1alpha1.RuleMatch{Servers: []string{"mail"}}, nil))
		decision := decisions.Decide(policy.Request{Identity: laptopUser(), Template: template("mail", anyone(), nil), State: nothing})
		assert.Equal(t, policy.StepRule, decision.Step)
	})

	t.Run("the scope refuses before approval", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{
			Identity: laptopUser(), Template: template("mail", anyone(), map[string]v1alpha1.ToolOverride{"send": {Approval: policy.ApprovalRequired}}),
			Tool: "send", State: nothing,
		})
		assert.False(t, decision.Allowed)
		assert.False(t, decision.NeedsApproval)
		assert.Equal(t, policy.StepWorkspaceScope, decision.Step)
	})

	t.Run("a claimed workspace gets the unverified message", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{Identity: laptopUser(), Template: template("immich", anyone(), nil), State: nothing})
		require.Equal(t, policy.StepWorkspaceScope, decision.Step)
		assert.Equal(t, "Server `immich` isn't allowed in sessions without a verified workspace. To allow all your servers in them, set the default for unverified sessions to all on the Workspaces page, or run `toolgate workspace defaults --unverified all`.", decision.Reason)
	})

	t.Run("a verified workspace gets the list message", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{Identity: verifiedUser(), Template: template("k8s", anyone(), nil), State: nothing})
		require.Equal(t, policy.StepWorkspaceScope, decision.Step)
		assert.Equal(t, "Server `k8s` isn't in the list for workspace `coder/alice/mail`. Allow it in the Workspaces page or with `toolgate workspace allow k8s`.", decision.Reason)
	})
}
