package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func workspaceEntry(server, tool, effect string) policy.ScopeEntry {
	return policy.ScopeEntry{Server: server, Tool: tool, Effect: effect}
}

func sessionEntry(server, tool, effect string) policy.ScopeEntry {
	return policy.ScopeEntry{Session: true, Server: server, Tool: tool, Effect: effect}
}

// decideScoped decides server and tool (empty for the whole server) with
// the scope built from rows.
func decideScoped(t *testing.T, identity auth.Identity, rows policy.ScopeRows, server, tool string) policy.Decision {
	t.Helper()

	state := policy.State{Scope: policy.EffectiveScope(identity, rows)}

	return engine(t).Decide(policy.Request{Identity: identity, Template: template(server, anyone(), nil), Tool: tool, State: state})
}

func TestStepFourScopeEntries(t *testing.T) {
	type decision struct {
		server, tool string
		allowed      bool
		denied       policy.DeniedIn
		reason       string
	}

	tests := []struct {
		name      string
		identity  auth.Identity
		rows      policy.ScopeRows
		decisions []decision
	}{
		{
			name:     "rule 1: a workspace deny of a tool refuses that tool even when the list allows the server",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", Entries: []policy.ScopeEntry{workspaceEntry("github", "delete_repository", policy.EffectDeny)}},
			decisions: []decision{
				{server: "github", tool: "delete_repository", denied: policy.DeniedInWorkspace, reason: "`github__delete_repository` is denied in workspace `coder/alice/mail`."},
				{server: "github", tool: "list_issues", allowed: true},
				{server: "github", allowed: true},
			},
		},
		{
			name:     "rule 1: a session deny of a tool says this session",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", Entries: []policy.ScopeEntry{sessionEntry("github", "delete_repository", policy.EffectDeny)}},
			decisions: []decision{
				{server: "github", tool: "delete_repository", denied: policy.DeniedInSession, reason: "`github__delete_repository` is denied in this session."},
			},
		},
		{
			name:     "rule 1: a server deny refuses the server and every tool, over a list, an addition and an allow entry",
			identity: verifiedUser(),
			rows: policy.ScopeRows{
				HasList: true, ListServers: []string{"github"}, Additions: []string{"github"},
				Entries: []policy.ScopeEntry{workspaceEntry("github", "", policy.EffectDeny), sessionEntry("github", "list_issues", policy.EffectAllow)},
			},
			decisions: []decision{
				{server: "github", denied: policy.DeniedInWorkspace, reason: "`github` is denied in workspace `coder/alice/mail`."},
				{server: "github", tool: "list_issues", denied: policy.DeniedInWorkspace, reason: "`github__list_issues` is denied in workspace `coder/alice/mail`."},
			},
		},
		{
			name:     "rule 1: a deny in both places names the workspace",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{VerifiedDefault: "all", Entries: []policy.ScopeEntry{sessionEntry("github", "", policy.EffectDeny), workspaceEntry("github", "", policy.EffectDeny)}},
			decisions: []decision{
				{server: "github", denied: policy.DeniedInWorkspace, reason: "`github` is denied in workspace `coder/alice/mail`."},
			},
		},
		{
			name:     "rule 2: the effective list allows the whole server",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{HasList: true, ListServers: []string{"github"}},
			decisions: []decision{
				{server: "github", allowed: true},
				{server: "github", tool: "anything", allowed: true},
			},
		},
		{
			name:     "rule 3: a workspace allow entry allows only its tool",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{HasList: true, Entries: []policy.ScopeEntry{workspaceEntry("k8s", "get_pods", policy.EffectAllow)}},
			decisions: []decision{
				{server: "k8s", tool: "get_pods", allowed: true},
				{server: "k8s", tool: "delete_pod", reason: "Server `k8s` isn't in the list for workspace `coder/alice/mail`. Allow it in the Workspaces page or with `toolgate workspace allow k8s`."},
				{server: "k8s", allowed: true},
				{server: "github"},
			},
		},
		{
			name:     "rule 3: a session allow entry allows only its tool",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{Entries: []policy.ScopeEntry{sessionEntry("k8s", "get_pods", policy.EffectAllow)}},
			decisions: []decision{
				{server: "k8s", tool: "get_pods", allowed: true},
				{server: "k8s", tool: "delete_pod"},
				{server: "k8s", allowed: true},
			},
		},
		{
			name:     "server level: an allow entry of a denied tool doesn't pass the server",
			identity: verifiedUser(),
			rows: policy.ScopeRows{Entries: []policy.ScopeEntry{
				workspaceEntry("k8s", "get_pods", policy.EffectAllow), sessionEntry("k8s", "get_pods", policy.EffectDeny),
			}},
			decisions: []decision{
				{server: "k8s"},
				{server: "k8s", tool: "get_pods", denied: policy.DeniedInSession, reason: "`k8s__get_pods` is denied in this session."},
			},
		},
		{
			name:     "rule 4: nothing allows it",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{},
			decisions: []decision{
				{server: "github"},
				{server: "github", tool: "list_issues"},
			},
		},
		{
			name:     "unverified: a claimed workspace never applies workspace entries",
			identity: laptopUser(),
			rows: policy.ScopeRows{UnverifiedDefault: "all", Entries: []policy.ScopeEntry{
				workspaceEntry("github", "", policy.EffectDeny), workspaceEntry("k8s", "get_pods", policy.EffectAllow),
			}},
			decisions: []decision{
				{server: "github", allowed: true},
				{server: "github", tool: "list_issues", allowed: true},
			},
		},
		{
			name:     "unverified: workspace allow entries never widen",
			identity: laptopUser(),
			rows:     policy.ScopeRows{Entries: []policy.ScopeEntry{workspaceEntry("k8s", "get_pods", policy.EffectAllow)}},
			decisions: []decision{
				{server: "k8s"},
				{server: "k8s", tool: "get_pods"},
			},
		},
		{
			name:     "unverified: session allow entries never widen",
			identity: laptopUser(),
			rows:     policy.ScopeRows{Entries: []policy.ScopeEntry{sessionEntry("k8s", "get_pods", policy.EffectAllow)}},
			decisions: []decision{
				{server: "k8s"},
				{server: "k8s", tool: "get_pods"},
			},
		},
		{
			name:     "unverified: session deny entries apply",
			identity: laptopUser(),
			rows: policy.ScopeRows{UnverifiedDefault: "all", Entries: []policy.ScopeEntry{
				sessionEntry("github", "", policy.EffectDeny), sessionEntry("k8s", "delete_pod", policy.EffectDeny),
			}},
			decisions: []decision{
				{server: "github", denied: policy.DeniedInSession, reason: "`github` is denied in this session."},
				{server: "k8s", allowed: true},
				{server: "k8s", tool: "delete_pod", denied: policy.DeniedInSession, reason: "`k8s__delete_pod` is denied in this session."},
				{server: "k8s", tool: "get_pods", allowed: true},
			},
		},
		{
			name:     "unverified list: the allow list is the effective list",
			identity: laptopUser(),
			rows:     policy.ScopeRows{UnverifiedDefault: "none", UnverifiedList: true, UnverifiedServers: []string{"github"}},
			decisions: []decision{
				{server: "github", allowed: true},
				{server: "github", tool: "list_issues", allowed: true},
				{server: "k8s"},
			},
		},
		{
			name:     "unverified list: list with no servers gives nothing",
			identity: laptopUser(),
			rows:     policy.ScopeRows{UnverifiedDefault: "none", UnverifiedList: true},
			decisions: []decision{
				{server: "github"},
			},
		},
		{
			name:     "unverified list: none ignores the stored servers",
			identity: laptopUser(),
			rows:     policy.ScopeRows{UnverifiedDefault: "none", UnverifiedServers: []string{"github"}},
			decisions: []decision{
				{server: "github"},
			},
		},
		{
			name:     "unverified list: a verified workspace ignores it",
			identity: verifiedUser(),
			rows:     policy.ScopeRows{UnverifiedDefault: "none", UnverifiedList: true, UnverifiedServers: []string{"github"}},
			decisions: []decision{
				{server: "github"},
			},
		},
		{
			name:     "service accounts ignore entries",
			identity: pod("ci"),
			rows:     policy.ScopeRows{Entries: []policy.ScopeEntry{workspaceEntry("github", "", policy.EffectDeny)}},
			decisions: []decision{
				{server: "github", allowed: true},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, expected := range test.decisions {
				got := decideScoped(t, test.identity, test.rows, expected.server, expected.tool)

				subject := expected.server + "/" + expected.tool
				if expected.allowed {
					assert.True(t, got.Allowed, "%s: %+v", subject, got)

					continue
				}

				require.False(t, got.Allowed, subject)
				assert.Equal(t, policy.StepWorkspaceScope, got.Step, subject)
				assert.Equal(t, expected.denied, got.Denied, subject)

				if expected.reason != "" {
					assert.Equal(t, expected.reason, got.Reason, subject)
				}
			}
		})
	}
}

// With the web UI off, the scope doesn't apply, so deny entries do nothing.
func TestScopeEntriesDoNothingWhenTheScopeIsOff(t *testing.T) {
	state := policy.State{Scope: policy.Scope{Denied: map[string]map[string]policy.DeniedIn{"github": {"": policy.DeniedInWorkspace}}}}
	decision := engine(t).Decide(policy.Request{Identity: verifiedUser(), Template: template("github", anyone(), nil), State: state})
	assert.True(t, decision.Allowed, "%+v", decision)
}
