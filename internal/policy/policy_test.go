package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

var now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func laptopUser() auth.Identity {
	return auth.Identity{
		Principal:   "alice-sub",
		Email:       "alice@example.com",
		Groups:      []string{"dev"},
		Workspace:   "claimed/default",
		Session:     "session-1",
		Kind:        auth.KindUser,
		Attestation: auth.AttestationUser,
	}
}

func pod(namespace string) auth.Identity {
	return auth.Identity{
		Principal:   "system:serviceaccount:" + namespace + ":runner",
		Workspace:   "k8s/" + namespace + "/ReplicaSet/runner-abc",
		Session:     "session-2",
		Kind:        auth.KindServiceAccount,
		Attestation: auth.AttestationAttested,
	}
}

func template(name string, access v1alpha1.Access, tools map[string]v1alpha1.ToolOverride) *v1alpha1.MCPServerTemplate {
	return &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "catalog"},
		Spec:       v1alpha1.MCPServerTemplateSpec{Scope: "per-session", Access: access, Tools: tools},
	}
}

func rule(name string, match v1alpha1.RuleMatch, expires *time.Time) *v1alpha1.ToolgateRule {
	toolgateRule := &v1alpha1.ToolgateRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rules"},
		Spec:       v1alpha1.ToolgateRuleSpec{Match: match, Reason: "reason of " + name},
	}
	if expires != nil {
		toolgateRule.Spec.Expires = &metav1.Time{Time: *expires}
	}

	return toolgateRule
}

func engine(t *testing.T, rules ...*v1alpha1.ToolgateRule) *policy.Engine {
	t.Helper()

	set := policy.NewRules()
	for _, toolgateRule := range rules {
		require.NoError(t, set.Set(toolgateRule))
	}

	return &policy.Engine{Rules: set, Now: func() time.Time { return now }}
}

func TestCatalogAccess(t *testing.T) {
	unverified := laptopUser()
	unverified.Email = ""

	fakeServiceAccountUser := laptopUser()
	fakeServiceAccountUser.Principal = "system:serviceaccount:ci:runner"

	tests := []struct {
		name     string
		identity auth.Identity
		access   v1alpha1.Access
		allowed  bool
	}{
		{name: "no access lists", identity: laptopUser(), access: v1alpha1.Access{Environments: "any"}},
		{name: "user by subject", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice-sub"}, Environments: "any"}, allowed: true},
		{name: "user by verified email", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice@example.com"}, Environments: "any"}, allowed: true},
		{name: "unverified email doesn't match", identity: unverified, access: v1alpha1.Access{Users: []string{"alice@example.com"}, Environments: "any"}},
		{name: "other user", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"bob-sub"}, Environments: "any"}},
		{name: "group", identity: laptopUser(), access: v1alpha1.Access{Groups: []string{"ops", "dev"}, Environments: "any"}, allowed: true},
		{name: "other group", identity: laptopUser(), access: v1alpha1.Access{Groups: []string{"ops"}, Environments: "any"}},
		{name: "users entries aren't globs", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"*"}, Environments: "any"}},
		{name: "service account", identity: pod("ci"), access: v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}, allowed: true},
		{name: "other service account", identity: pod("ci"), access: v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:ci:other"}, Environments: "any"}},
		{name: "service account listed as user", identity: pod("ci"), access: v1alpha1.Access{Users: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}},
		{name: "user subject named like a service account", identity: fakeServiceAccountUser, access: v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{Identity: test.identity, Template: template("github", test.access, nil)})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)

			if !test.allowed {
				assert.Equal(t, policy.StepAccess, decision.Step)
				assert.Contains(t, decision.Reason, `"github"`)
			}
		})
	}
}

func spiffeWorkload() auth.Identity {
	return auth.Identity{
		Principal:   "spiffe://coder.example.com/owner/alice/workspace/dev",
		Workspace:   "/owner/alice/workspace/dev",
		Session:     "session-3",
		Kind:        auth.KindSPIFFE,
		Attestation: auth.AttestationAttested,
	}
}

func TestSPIFFECatalogAccess(t *testing.T) {
	spiffeUser := laptopUser()
	spiffeUser.Principal = "spiffe://coder.example.com/owner/alice/workspace/dev"

	tests := []struct {
		name     string
		identity auth.Identity
		access   v1alpha1.Access
		allowed  bool
	}{
		{name: "exact ID", identity: spiffeWorkload(), access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://coder.example.com/owner/alice/workspace/dev"}}, allowed: true},
		{name: "glob", identity: spiffeWorkload(), access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://coder.example.com/owner/*/workspace/*"}}, allowed: true},
		{name: "glob of another trust domain", identity: spiffeWorkload(), access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://other.test/*"}}},
		{name: "invalid pattern matches nothing", identity: spiffeWorkload(), access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://coder.example.com/[*"}}},
		{name: "SPIFFE ID listed as user", identity: spiffeWorkload(), access: v1alpha1.Access{Users: []string{"spiffe://coder.example.com/owner/alice/workspace/dev"}}},
		{name: "SPIFFE ID listed as service account", identity: spiffeWorkload(), access: v1alpha1.Access{ServiceAccounts: []string{"spiffe://coder.example.com/owner/alice/workspace/dev"}}},
		{name: "SPIFFE ID with a group list", identity: spiffeWorkload(), access: v1alpha1.Access{Groups: []string{""}, Users: []string{"*"}}},
		{name: "user subject named like a SPIFFE ID", identity: spiffeUser, access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://*"}, Environments: "any"}},
		{name: "service account doesn't match spiffeIDs", identity: pod("ci"), access: v1alpha1.Access{SPIFFEIDs: []string{"*"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{Identity: test.identity, Template: template("github", test.access, nil)})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)

			if !test.allowed {
				assert.Equal(t, policy.StepAccess, decision.Step)
				assert.Contains(t, decision.Reason, `"github"`)
			}
		})
	}
}

func TestKindPicksAccessList(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		access    v1alpha1.Access
		allowed   bool
	}{
		{name: "SPIFFE-like subject in users", principal: "spiffe://coder.example.com/alice", access: v1alpha1.Access{Users: []string{"spiffe://coder.example.com/alice"}}, allowed: true},
		{name: "SPIFFE-like subject by group", principal: "spiffe://coder.example.com/alice", access: v1alpha1.Access{Groups: []string{"dev"}}, allowed: true},
		{name: "SPIFFE-like subject in spiffeIDs", principal: "spiffe://coder.example.com/alice", access: v1alpha1.Access{SPIFFEIDs: []string{"spiffe://*"}}},
		{name: "service-account-like subject in users", principal: "system:serviceaccount:ci:runner", access: v1alpha1.Access{Users: []string{"system:serviceaccount:ci:runner"}}, allowed: true},
		{name: "service-account-like subject by group", principal: "system:serviceaccount:ci:runner", access: v1alpha1.Access{Groups: []string{"dev"}}, allowed: true},
		{name: "service-account-like subject in serviceAccounts", principal: "system:serviceaccount:ci:runner", access: v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:ci:runner"}}},
	}

	for _, test := range tests {
		for _, attestation := range []string{auth.AttestationUser, auth.AttestationAttested} {
			t.Run(test.name+" at "+attestation, func(t *testing.T) {
				identity := auth.Identity{Principal: test.principal, Groups: []string{"dev"}, Kind: auth.KindUser, Attestation: attestation}

				assert.Equal(t, test.allowed, policy.HasAccess(identity, test.access))
			})
		}
	}

	t.Run("no kind matches nothing", func(t *testing.T) {
		identity := auth.Identity{Principal: "alice-sub", Groups: []string{"dev"}, Attestation: auth.AttestationUser}
		access := v1alpha1.Access{Users: []string{"alice-sub"}, Groups: []string{"dev"}, ServiceAccounts: []string{"alice-sub"}, SPIFFEIDs: []string{"*"}}

		assert.False(t, policy.HasAccess(identity, access))
	})
}

func TestRuleMatchesSPIFFEWorkspace(t *testing.T) {
	scratch := spiffeWorkload()
	scratch.Principal = "spiffe://coder.example.com/owner/alice/workspace/scratch-1"
	scratch.Workspace = "/owner/alice/workspace/scratch-1"

	rules := engine(t, rule("no-scratch", v1alpha1.RuleMatch{
		Workspaces: []string{"/owner/*/workspace/scratch-*"}, Attestation: "attested",
	}, nil))
	access := v1alpha1.Access{SPIFFEIDs: []string{"spiffe://coder.example.com/*"}}

	decision := rules.Decide(policy.Request{Identity: scratch, Template: template("github", access, nil)})
	assert.False(t, decision.Allowed)
	assert.Equal(t, policy.StepRule, decision.Step)

	decision = rules.Decide(policy.Request{Identity: spiffeWorkload(), Template: template("github", access, nil)})
	assert.True(t, decision.Allowed, decision.Reason)
}

func TestAttestationRules(t *testing.T) {
	access := func(environments string) v1alpha1.Access {
		return v1alpha1.Access{Users: []string{"alice-sub"}, ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: environments}
	}

	tests := []struct {
		name         string
		environments string
		identity     auth.Identity
		allowed      bool
	}{
		{name: "any, user", environments: "any", identity: laptopUser(), allowed: true},
		{name: "any, attested", environments: "any", identity: pod("ci"), allowed: true},
		{name: "opt-in, user", environments: "opt-in", identity: laptopUser()},
		{name: "opt-in, attested", environments: "opt-in", identity: pod("ci"), allowed: true},
		{name: "attested, user", environments: "attested", identity: laptopUser()},
		{name: "attested, attested", environments: "attested", identity: pod("ci"), allowed: true},
		{name: "omitted means opt-in, user", environments: "", identity: laptopUser()},
		{name: "omitted means opt-in, attested", environments: "", identity: pod("ci"), allowed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{Identity: test.identity, Template: template("github", access(test.environments), nil)})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)

			if !test.allowed {
				assert.Equal(t, policy.StepEnvironment, decision.Step)
			}
		})
	}
}

func TestToolOverrides(t *testing.T) {
	tools := map[string]v1alpha1.ToolOverride{
		"delete_repository": {Environments: "attested"},
		"list_issues":       {Environments: "any"},
		"list_secrets":      {Blocked: true},
	}
	anyServer := template("github", v1alpha1.Access{Users: []string{"alice-sub"}, ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}, tools)
	attestedServer := template("github", v1alpha1.Access{Users: []string{"alice-sub"}, Environments: "attested"}, tools)

	tests := []struct {
		name     string
		template *v1alpha1.MCPServerTemplate
		identity auth.Identity
		tool     string
		allowed  bool
		step     string
	}{
		{name: "tool override attested refuses user", template: anyServer, identity: laptopUser(), tool: "delete_repository", step: policy.StepEnvironment},
		{name: "tool override attested allows attested", template: anyServer, identity: pod("ci"), tool: "delete_repository", allowed: true},
		{name: "override doesn't reach other tools", template: anyServer, identity: laptopUser(), tool: "create_issue", allowed: true},
		{name: "override doesn't reach the server", template: anyServer, identity: laptopUser(), allowed: true},
		{name: "tool override any widens an attested server", template: attestedServer, identity: laptopUser(), tool: "list_issues", allowed: true},
		{name: "attested server refuses tools without override", template: attestedServer, identity: laptopUser(), tool: "create_issue", step: policy.StepEnvironment},
		{name: "blocked tool", template: anyServer, identity: pod("ci"), tool: "list_secrets", step: policy.StepBlocked},
		{name: "blocked doesn't reach other tools", template: anyServer, identity: pod("ci"), tool: "list_issues", allowed: true},
		{name: "blocked doesn't reach the server", template: anyServer, identity: pod("ci"), allowed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{Identity: test.identity, Template: test.template, Tool: test.tool})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)
			assert.Equal(t, test.step, decision.Step)

			if test.tool != "" && !test.allowed {
				assert.Contains(t, decision.Reason, "github__"+test.tool)
			}
		})
	}
}

func TestPolicyRules(t *testing.T) {
	everyone := v1alpha1.Access{Users: []string{"alice-sub"}, Groups: []string{"dev"}, ServiceAccounts: []string{"system:serviceaccount:ci:runner", "system:serviceaccount:ci2:runner", "system:serviceaccount:prod:runner"}, Environments: "any"}
	github := template("github", everyone, nil)
	jira := template("jira", everyone, nil)
	expiry := now.Add(time.Hour)
	expiredAt := now
	expiresAfter := now.Add(time.Nanosecond)

	blockCI := rule("block-ci", v1alpha1.RuleMatch{Workspaces: []string{"k8s/ci/*"}, Servers: []string{"github"}}, nil)

	tests := []struct {
		name     string
		rules    []*v1alpha1.ToolgateRule
		identity auth.Identity
		template *v1alpha1.MCPServerTemplate
		tool     string
		refused  string
	}{
		{name: "workspace glob matches across slashes", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: pod("ci"), template: github, refused: "rules/block-ci"},
		{name: "workspace glob matches a call", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: pod("ci"), template: github, tool: "create_issue", refused: "rules/block-ci"},
		{name: "other namespace", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: pod("prod"), template: github},
		{name: "namespace prefix isn't a match", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: pod("ci2"), template: github},
		{name: "laptop user", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: laptopUser(), template: github},
		{name: "other server", rules: []*v1alpha1.ToolgateRule{blockCI}, identity: pod("ci"), template: jira},
		{
			name:     "tool glob matches the proxied name",
			rules:    []*v1alpha1.ToolgateRule{rule("no-delete", v1alpha1.RuleMatch{Tools: []string{"github__delete_*"}}, nil)},
			identity: laptopUser(), template: github, tool: "delete_repository", refused: "rules/no-delete",
		},
		{
			name:     "tool glob leaves other tools",
			rules:    []*v1alpha1.ToolgateRule{rule("no-delete", v1alpha1.RuleMatch{Tools: []string{"github__delete_*"}}, nil)},
			identity: laptopUser(), template: github, tool: "create_issue",
		},
		{
			name:     "tool rule leaves the server enableable",
			rules:    []*v1alpha1.ToolgateRule{rule("no-delete", v1alpha1.RuleMatch{Tools: []string{"github__delete_*"}}, nil)},
			identity: laptopUser(), template: github,
		},
		{
			name:     "tool wildcard leaves the server enableable",
			rules:    []*v1alpha1.ToolgateRule{rule("no-tools", v1alpha1.RuleMatch{Tools: []string{"*"}}, nil)},
			identity: laptopUser(), template: github,
		},
		{
			name:     "every listed field must match",
			rules:    []*v1alpha1.ToolgateRule{rule("ci-delete", v1alpha1.RuleMatch{Workspaces: []string{"k8s/ci/*"}, Tools: []string{"github__delete_*"}}, nil)},
			identity: pod("prod"), template: github, tool: "delete_repository",
		},
		{
			name:     "all fields matching",
			rules:    []*v1alpha1.ToolgateRule{rule("ci-delete", v1alpha1.RuleMatch{Workspaces: []string{"k8s/ci/*"}, Tools: []string{"github__delete_*"}}, nil)},
			identity: pod("ci"), template: github, tool: "delete_repository", refused: "rules/ci-delete",
		},
		{
			name:     "principal by verified email",
			rules:    []*v1alpha1.ToolgateRule{rule("alice", v1alpha1.RuleMatch{Principals: []string{"alice@*"}}, nil)},
			identity: laptopUser(), template: github, refused: "rules/alice",
		},
		{
			name:     "group",
			rules:    []*v1alpha1.ToolgateRule{rule("dev", v1alpha1.RuleMatch{Groups: []string{"dev"}}, nil)},
			identity: laptopUser(), template: github, refused: "rules/dev",
		},
		{
			name:     "claimed session",
			rules:    []*v1alpha1.ToolgateRule{rule("session", v1alpha1.RuleMatch{Sessions: []string{"session-1"}}, nil)},
			identity: laptopUser(), template: github, refused: "rules/session",
		},
		{
			name:     "attestation",
			rules:    []*v1alpha1.ToolgateRule{rule("users", v1alpha1.RuleMatch{Attestation: "user"}, nil)},
			identity: pod("ci"), template: github,
		},
		{
			name:     "empty match refuses everything",
			rules:    []*v1alpha1.ToolgateRule{rule("all", v1alpha1.RuleMatch{}, nil)},
			identity: pod("prod"), template: jira, refused: "rules/all",
		},
		{
			name:     "before expires",
			rules:    []*v1alpha1.ToolgateRule{rule("temporary", v1alpha1.RuleMatch{Servers: []string{"github"}}, &expiry)},
			identity: laptopUser(), template: github, refused: "rules/temporary",
		},
		{
			name:     "one nanosecond before expires",
			rules:    []*v1alpha1.ToolgateRule{rule("temporary", v1alpha1.RuleMatch{Servers: []string{"github"}}, &expiresAfter)},
			identity: laptopUser(), template: github, refused: "rules/temporary",
		},
		{
			name:     "at expires",
			rules:    []*v1alpha1.ToolgateRule{rule("temporary", v1alpha1.RuleMatch{Servers: []string{"github"}}, &expiredAt)},
			identity: laptopUser(), template: github,
		},
		{
			name:     "first matching rule by name",
			rules:    []*v1alpha1.ToolgateRule{rule("zz", v1alpha1.RuleMatch{}, nil), rule("aa", v1alpha1.RuleMatch{}, nil)},
			identity: laptopUser(), template: github, refused: "rules/aa",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t, test.rules...).Decide(policy.Request{Identity: test.identity, Template: test.template, Tool: test.tool})

			if test.refused == "" {
				assert.True(t, decision.Allowed, decision.Reason)

				return
			}

			assert.False(t, decision.Allowed)
			assert.Equal(t, policy.StepRule, decision.Step)
			assert.Equal(t, test.refused, decision.Rule)
			assert.Contains(t, decision.Reason, test.refused)
			assert.Contains(t, decision.Reason, "reason of ")
		})
	}
}

func TestApprovalRequired(t *testing.T) {
	tools := map[string]v1alpha1.ToolOverride{
		"delete_repository": {Approval: "required"},
		"wipe":              {Approval: "required", Blocked: true},
	}
	server := template("github", v1alpha1.Access{Users: []string{"alice-sub"}, ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}, tools)

	tests := []struct {
		name          string
		identity      auth.Identity
		tool          string
		allowed       bool
		needsApproval bool
	}{
		{name: "user needs approval", identity: laptopUser(), tool: "delete_repository", allowed: true, needsApproval: true},
		{name: "attested needs approval", identity: pod("ci"), tool: "delete_repository", allowed: true, needsApproval: true},
		{name: "other tools need none", identity: laptopUser(), tool: "list_issues", allowed: true},
		{name: "the server needs none", identity: laptopUser(), allowed: true},
		{name: "a block refuses first", identity: laptopUser(), tool: "wipe"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{Identity: test.identity, Template: server, Tool: test.tool})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)
			assert.Equal(t, test.needsApproval, decision.NeedsApproval)
		})
	}
}

func TestAccessIsCheckedBeforeRules(t *testing.T) {
	decision := engine(t, rule("all", v1alpha1.RuleMatch{}, nil)).Decide(policy.Request{
		Identity: laptopUser(),
		Template: template("github", v1alpha1.Access{Environments: "any"}, nil),
	})

	assert.Equal(t, policy.StepAccess, decision.Step)
}

func TestRulesSet(t *testing.T) {
	tests := []struct {
		name        string
		match       v1alpha1.RuleMatch
		assertError require.ErrorAssertionFunc
	}{
		{name: "valid", match: v1alpha1.RuleMatch{Workspaces: []string{"k8s/ci/*"}, Tools: []string{"github__{create,delete}_*"}}},
		{name: "glob that doesn't compile", match: v1alpha1.RuleMatch{Tools: []string{"github__[delete"}}, assertError: require.Error},
		{name: "unknown attestation", match: v1alpha1.RuleMatch{Attestation: "root"}, assertError: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			assertError(t, policy.NewRules().Set(rule("rule", test.match, nil)))
		})
	}
}

func TestInvalidRuleKeepsLastValidGeneration(t *testing.T) {
	rules := policy.NewRules()
	github := template("github", v1alpha1.Access{Users: []string{"alice-sub"}, Environments: "any"}, nil)
	policyEngine := &policy.Engine{Rules: rules, Now: func() time.Time { return now }}

	invalid := rule("rule", v1alpha1.RuleMatch{Servers: []string{"[github"}}, nil)
	require.Error(t, rules.Set(invalid))
	assert.True(t, policyEngine.Decide(policy.Request{Identity: laptopUser(), Template: github}).Allowed, "an invalid rule applies to nothing")

	require.NoError(t, rules.Set(rule("rule", v1alpha1.RuleMatch{Servers: []string{"github"}}, nil)))
	require.Error(t, rules.Set(invalid))
	assert.Equal(t, "rules/rule", policyEngine.Decide(policy.Request{Identity: laptopUser(), Template: github}).Rule)
}
