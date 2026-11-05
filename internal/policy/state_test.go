package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func everyone() v1alpha1.Access {
	return v1alpha1.Access{Users: []string{"alice@example.com"}, ServiceAccounts: []string{"system:serviceaccount:ci:runner"}}
}

func serverGrant(id string) policy.Grant {
	return policy.Grant{ID: id, Principal: "alice-sub", TemplateNamespace: "catalog", TemplateName: "github"}
}

func TestDecideGrants(t *testing.T) {
	optInServer := everyone()
	optInServer.Environments = policy.EnvironmentOptIn

	anyServer := everyone()
	anyServer.Environments = policy.EnvironmentAny

	attestedServer := everyone()
	attestedServer.Environments = policy.EnvironmentAttested

	optInTool := map[string]v1alpha1.ToolOverride{"push": {Environments: policy.EnvironmentOptIn}}

	toolGrant := serverGrant("tool-grant")
	toolGrant.Tool = "push"

	otherToolGrant := serverGrant("other-tool-grant")
	otherToolGrant.Tool = "pull"

	narrowed := serverGrant("narrowed")
	narrowed.Session = "session-1"

	otherSession := serverGrant("other-session")
	otherSession.Session = "session-9"

	otherPrincipal := serverGrant("other-principal")
	otherPrincipal.Principal = "bob-sub"

	otherServer := serverGrant("other-server")
	otherServer.TemplateName = "gitlab"

	otherNamespace := serverGrant("other-namespace")
	otherNamespace.TemplateNamespace = "elsewhere"

	ci := pod("ci")

	tests := []struct {
		name      string
		identity  auth.Identity
		access    v1alpha1.Access
		tools     map[string]v1alpha1.ToolOverride
		tool      string
		grants    []policy.Grant
		allowed   bool
		wantGrant string
	}{
		{name: "opt-in server without a grant refuses a user", identity: laptopUser(), access: optInServer},
		{name: "opt-in server with a whole-server grant", identity: laptopUser(), access: optInServer, grants: []policy.Grant{serverGrant("g1")}, allowed: true, wantGrant: "g1"},
		{name: "a whole-server grant covers a tool of an opt-in server", identity: laptopUser(), access: optInServer, tool: "push", grants: []policy.Grant{serverGrant("g1")}, allowed: true, wantGrant: "g1"},
		{name: "a tool grant passes its tool", identity: laptopUser(), access: anyServer, tools: optInTool, tool: "push", grants: []policy.Grant{toolGrant}, allowed: true, wantGrant: "tool-grant"},
		{name: "a whole-server grant covers an opt-in tool", identity: laptopUser(), access: anyServer, tools: optInTool, tool: "push", grants: []policy.Grant{serverGrant("g1")}, allowed: true, wantGrant: "g1"},
		{name: "a grant of another tool doesn't pass", identity: laptopUser(), access: anyServer, tools: optInTool, tool: "push", grants: []policy.Grant{otherToolGrant}},
		{name: "a tool grant doesn't count for the whole server", identity: laptopUser(), access: optInServer, grants: []policy.Grant{toolGrant}},
		{name: "a grant narrowed to this session", identity: laptopUser(), access: optInServer, grants: []policy.Grant{narrowed}, allowed: true, wantGrant: "narrowed"},
		{name: "a grant narrowed to another session", identity: laptopUser(), access: optInServer, grants: []policy.Grant{otherSession}},
		{name: "a grant of another principal", identity: laptopUser(), access: optInServer, grants: []policy.Grant{otherPrincipal}},
		{name: "a grant of another server", identity: laptopUser(), access: optInServer, grants: []policy.Grant{otherServer}},
		{name: "a grant of a server in another namespace", identity: laptopUser(), access: optInServer, grants: []policy.Grant{otherNamespace}},
		{name: "a grant doesn't pass attested", identity: laptopUser(), access: attestedServer, grants: []policy.Grant{serverGrant("g1")}},
		{name: "any passes without a grant", identity: laptopUser(), access: anyServer, allowed: true},
		{name: "opt-in passes attested without a grant", identity: ci, access: optInServer, allowed: true},
		{name: "first matching grant is recorded", identity: laptopUser(), access: optInServer, grants: []policy.Grant{otherSession, serverGrant("g2"), serverGrant("g3")}, allowed: true, wantGrant: "g2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := engine(t).Decide(policy.Request{
				Identity: test.identity,
				Template: template("github", test.access, test.tools),
				Tool:     test.tool,
				State:    policy.State{Grants: test.grants},
			})

			assert.Equal(t, test.allowed, decision.Allowed, decision.Reason)
			assert.Equal(t, test.wantGrant, decision.Grant)

			if !test.allowed {
				assert.Equal(t, policy.StepEnvironment, decision.Step)
			}
		})
	}
}

func TestDecideMissingGrantLinksToTheGrantsPage(t *testing.T) {
	access := everyone()
	access.Environments = policy.EnvironmentOptIn

	var asked []string

	decide := func(grantURL func(server, tool, session string) string, tool string, tools map[string]v1alpha1.ToolOverride) policy.Decision {
		decider := engine(t)
		decider.GrantURL = grantURL

		return decider.Decide(policy.Request{Identity: laptopUser(), Template: template("github", access, tools), Tool: tool})
	}

	link := func(server, tool, session string) string {
		asked = append(asked, server+"|"+tool+"|"+session)

		return "https://toolgate.example.com/ui/grants/new?server=" + server + "&tool=" + tool + "&session=" + session
	}

	server := decide(link, "", nil)
	assert.Equal(t, policy.StepEnvironment, server.Step)
	assert.Equal(t, "Server `github` needs a grant on a signed-in user's machine. Create one at https://toolgate.example.com/ui/grants/new?server=github&tool=&session=session-1, then try again.", server.Reason)

	tool := decide(link, "push", nil)
	assert.Equal(t, "Tool `github__push` needs a grant on a signed-in user's machine. Create one at https://toolgate.example.com/ui/grants/new?server=github&tool=push&session=session-1, then try again.", tool.Reason)
	assert.Equal(t, []string{"github||session-1", "github|push|session-1"}, asked)

	withoutUI := decide(nil, "", nil)
	assert.Equal(t, `Server "github" can be used only from an attested environment, not from a signed-in user's machine.`, withoutUI.Reason)

	attestedTool := decide(link, "push", map[string]v1alpha1.ToolOverride{"push": {Environments: policy.EnvironmentAttested}})
	assert.Equal(t, `Tool "github__push" can be used only from an attested environment, not from a signed-in user's machine.`, attestedTool.Reason,
		"a grant can't pass attested, so no link")
}

func block(t *testing.T, id string, match v1alpha1.RuleMatch) *policy.TemporaryBlock {
	t.Helper()

	created, err := policy.NewTemporaryBlock(id, match, "reason of "+id, time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	return created
}

func TestDecideTemporaryBlocks(t *testing.T) {
	access := everyone()
	access.Environments = policy.EnvironmentAny

	blockedTool := map[string]v1alpha1.ToolOverride{"push": {Blocked: true}}

	tests := []struct {
		name      string
		tool      string
		tools     map[string]v1alpha1.ToolOverride
		rules     []*v1alpha1.ToolgateRule
		blocks    []v1alpha1.RuleMatch
		wantStep  string
		wantBlock string
	}{
		{name: "no blocks"},
		{name: "a block that doesn't match", blocks: []v1alpha1.RuleMatch{{Servers: []string{"gitlab"}}}},
		{name: "a server block", blocks: []v1alpha1.RuleMatch{{Servers: []string{"git*"}}}, wantStep: policy.StepTemporaryBlock, wantBlock: "b0"},
		{name: "a tool block refuses its tool", tool: "push", blocks: []v1alpha1.RuleMatch{{Tools: []string{"github__push"}}}, wantStep: policy.StepTemporaryBlock, wantBlock: "b0"},
		{name: "a tool block doesn't refuse the whole server", blocks: []v1alpha1.RuleMatch{{Tools: []string{"github__push"}}}},
		{name: "a block by email", blocks: []v1alpha1.RuleMatch{{Principals: []string{"alice@*"}}}, wantStep: policy.StepTemporaryBlock, wantBlock: "b0"},
		{name: "a block by another attestation", blocks: []v1alpha1.RuleMatch{{Attestation: auth.AttestationAttested}}},
		{name: "the oldest matching block refuses", blocks: []v1alpha1.RuleMatch{{Servers: []string{"gitlab"}}, {Servers: []string{"github"}}, {Groups: []string{"dev"}}}, wantStep: policy.StepTemporaryBlock, wantBlock: "b1"},
		{name: "the template's blocked flag comes first", tool: "push", tools: blockedTool, blocks: []v1alpha1.RuleMatch{{Servers: []string{"github"}}}, wantStep: policy.StepBlocked},
		{
			name:     "a rule comes before a temporary block",
			rules:    []*v1alpha1.ToolgateRule{rule("standing", v1alpha1.RuleMatch{Servers: []string{"github"}}, nil)},
			blocks:   []v1alpha1.RuleMatch{{Servers: []string{"github"}}},
			wantStep: policy.StepRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state policy.State
			for index, match := range test.blocks {
				state.Blocks = append(state.Blocks, block(t, "b"+string(rune('0'+index)), match))
			}

			decision := engine(t, test.rules...).Decide(policy.Request{
				Identity: laptopUser(), Template: template("github", access, test.tools), Tool: test.tool, State: state,
			})

			assert.Equal(t, test.wantStep == "", decision.Allowed, decision.Reason)
			assert.Equal(t, test.wantStep, decision.Step)
			assert.Equal(t, test.wantBlock, decision.Block)

			if test.wantBlock != "" {
				assert.Equal(t, "Blocked by an admin until 2026-11-05T14:00:00Z: reason of "+test.wantBlock+" (block "+test.wantBlock+").", decision.Reason)
			}
		})
	}
}

func TestDecideTemporaryBlockAfterGrant(t *testing.T) {
	access := everyone()
	access.Environments = policy.EnvironmentOptIn

	decision := engine(t).Decide(policy.Request{
		Identity: laptopUser(), Template: template("github", access, nil),
		State: policy.State{Grants: []policy.Grant{serverGrant("g1")}, Blocks: []*policy.TemporaryBlock{block(t, "b0", v1alpha1.RuleMatch{Servers: []string{"github"}})}},
	})

	assert.Equal(t, policy.StepTemporaryBlock, decision.Step, "a grant passes step 2 only; the block still refuses")
	assert.Equal(t, "b0", decision.Block)
}

func TestNewTemporaryBlockRefusesBadGlobs(t *testing.T) {
	_, err := policy.NewTemporaryBlock("b0", v1alpha1.RuleMatch{Servers: []string{"[unclosed"}}, "reason", time.Now())
	require.Error(t, err)

	_, err = policy.NewTemporaryBlock("b0", v1alpha1.RuleMatch{Attestation: "maybe"}, "reason", time.Now())
	require.Error(t, err)
}
