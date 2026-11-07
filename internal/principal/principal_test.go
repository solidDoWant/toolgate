package principal_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/principal"
)

var hashed = strings.Repeat("914009fe", 8)

func TestOfShowsEachKindOfPrincipalAsPeopleReadIt(t *testing.T) {
	tests := []struct {
		name         string
		principal    string
		profile      *principal.Profile
		self         string
		wantText     string
		wantHover    string
		wantCopyable bool
	}{
		{name: "name and email", principal: hashed, profile: &principal.Profile{Name: "Fred", Email: "fred@example.com"}, wantText: "Fred <fred@example.com>", wantHover: hashed},
		{name: "email only", principal: hashed, profile: &principal.Profile{Email: "fred@example.com"}, wantText: "fred@example.com", wantHover: hashed},
		{name: "name only when the email is unverified", principal: hashed, profile: &principal.Profile{Name: "Fred"}, wantText: "Fred", wantHover: hashed},
		{name: "username last", principal: hashed, profile: &principal.Profile{Username: "fred"}, wantText: "fred", wantHover: hashed},
		{name: "never seen", principal: hashed, wantText: "user 914009fe…", wantHover: hashed, wantCopyable: true},
		{name: "empty profile", principal: hashed, profile: &principal.Profile{}, wantText: "user 914009fe…", wantHover: hashed, wantCopyable: true},
		{name: "short unknown subject is kept", principal: "alice", wantText: "user alice", wantHover: "alice", wantCopyable: true},
		{name: "own principal with a name", principal: hashed, profile: &principal.Profile{Name: "Fred", Email: "fred@example.com"}, self: hashed, wantText: "you", wantHover: "Fred <fred@example.com> (you)\n" + hashed},
		{name: "own principal never seen", principal: hashed, self: hashed, wantText: "you", wantHover: hashed},
		{name: "service account", principal: "system:serviceaccount:ci:runner", wantText: "serviceaccount ci/runner", wantHover: "system:serviceaccount:ci:runner"},
		{name: "short spiffe id", principal: "spiffe://example.org/ns/a", wantText: "spiffe://example.org/ns/a", wantHover: "spiffe://example.org/ns/a"},
		{name: "long spiffe id", principal: "spiffe://example.org/" + strings.Repeat("x", 80), wantText: "spiffe://example.org/" + strings.Repeat("x", 26) + "…", wantHover: "spiffe://example.org/" + strings.Repeat("x", 80)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			display := principal.Of(test.principal, test.profile, test.self)

			assert.Equal(t, test.wantText, display.Text)
			assert.Equal(t, test.wantHover, display.Hover)
			assert.Equal(t, test.principal, display.Raw)
			assert.Equal(t, test.wantCopyable, display.Copyable)
		})
	}
}

func TestShortKeepsTheStartOfALongID(t *testing.T) {
	assert.Equal(t, "914009fe…", principal.Short(hashed))
	assert.Equal(t, "123456789", principal.Short("123456789"))
	assert.Equal(t, "12345678…", principal.Short("1234567890"))
}

func TestWorkspaceLabelsKnownForms(t *testing.T) {
	assert.Equal(t, "Coder workspace toolgate-auto", principal.Workspace("coder/mydudeiamhere/toolgate-auto"))
	assert.Equal(t, "unverified: default", principal.Workspace("claimed/default"))
	assert.Empty(t, principal.Workspace("k8s/ns/Deployment/x"))
}
