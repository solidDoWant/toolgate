package policy

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/changes"
)

// Rules holds the valid ToolgateRule objects read from the watched
// namespaces, with their globs compiled. A rule whose new generation is
// invalid keeps its last valid generation. It is safe for concurrent use.
//
// Rules keeps a timer for the earliest future expires time of its valid
// rules. When the timer fires, Rules signals its subscribers, because the
// rules that apply have changed, and sets the timer for the next one.
type Rules struct {
	clock clock.WithDelayedExecution

	mu      sync.RWMutex
	entries map[types.NamespacedName]*ruleEntry
	// expiry fires at expiryAt, the earliest future expires time, or is nil
	// when no valid rule expires in the future.
	expiry   clock.Timer
	expiryAt time.Time

	changes changes.Feed
}

type ruleEntry struct {
	// valid is the latest valid generation, compiled, or nil.
	valid *compiledRule
}

type compiledRule struct {
	key         types.NamespacedName
	reason      string
	expires     *time.Time
	principals  []glob.Glob
	groups      []glob.Glob
	workspaces  []glob.Glob
	sessions    []glob.Glob
	attestation string
	servers     []glob.Glob
	tools       []glob.Glob
}

// NewRules returns an empty rule set that times expiry with the real clock.
func NewRules() *Rules {
	return NewRulesWithClock(clock.RealClock{})
}

// NewRulesWithClock returns an empty rule set that times expiry with the
// given clock.
func NewRulesWithClock(timing clock.WithDelayedExecution) *Rules {
	return &Rules{clock: timing, entries: map[types.NamespacedName]*ruleEntry{}}
}

// Set records the latest generation of a rule. A rule with a glob that
// doesn't compile is invalid: Set returns the reason and keeps applying the
// rule's last valid generation, if any.
func (r *Rules) Set(rule *v1alpha1.ToolgateRule) error {
	key := types.NamespacedName{Namespace: rule.Namespace, Name: rule.Name}
	compiled, err := compile(key, rule)

	r.mu.Lock()

	current, found := r.entries[key]
	if !found {
		current = &ruleEntry{}
		r.entries[key] = current
	}

	if err != nil {
		r.mu.Unlock()

		return err
	}

	current.valid = compiled

	r.scheduleExpiryLocked()
	r.mu.Unlock()

	r.changes.Notify()

	return nil
}

// Delete forgets a rule.
func (r *Rules) Delete(key types.NamespacedName) {
	r.mu.Lock()
	delete(r.entries, key)
	r.scheduleExpiryLocked()
	r.mu.Unlock()

	r.changes.Notify()
}

// Subscribe returns a channel that receives after the rules that apply
// change, and a function that ends the subscription. Signals not yet
// received are merged into one.
func (r *Rules) Subscribe() (<-chan struct{}, func()) {
	return r.changes.Subscribe()
}

// scheduleExpiryLocked sets the expiry timer to the earliest expires time
// after now, or stops it when no valid rule expires in the future. The
// caller holds r.mu.
func (r *Rules) scheduleExpiryLocked() {
	now := r.clock.Now()

	var next time.Time

	for _, current := range r.entries {
		rule := current.valid
		if rule == nil || rule.expires == nil || !rule.expires.After(now) {
			continue
		}

		if next.IsZero() || rule.expires.Before(next) {
			next = *rule.expires
		}
	}

	if r.expiry != nil && next.Equal(r.expiryAt) {
		return
	}

	if r.expiry != nil {
		r.expiry.Stop()
		r.expiry = nil
	}

	r.expiryAt = next
	if next.IsZero() {
		return
	}

	// The callback starts a goroutine because a clock may run it while
	// holding a lock that Now and AfterFunc also take.
	r.expiry = r.clock.AfterFunc(next.Sub(now), func() { go r.expired() })
}

// expired runs when the expiry timer fires: a rule has stopped applying.
func (r *Rules) expired() {
	r.mu.Lock()
	r.expiry = nil
	r.expiryAt = time.Time{}
	r.scheduleExpiryLocked()
	r.mu.Unlock()

	r.changes.Notify()
}

// target is what a rule is matched against.
type target struct {
	principal   string
	email       string
	groups      []string
	workspace   string
	session     string
	attestation string
	server      string
	// tool is the proxied name, <server>__<tool>, or empty for a request
	// about the whole server.
	tool string
	// exposedTool is the proxied name with the exposed tool name, when it
	// differs from tool. A rule's tools glob matches either name.
	exposedTool string
}

// match returns the first rule, by namespace and name, that matches the
// target and hasn't expired at now.
func (r *Rules) match(request target, now time.Time) (*compiledRule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var matched *compiledRule

	for _, current := range r.entries {
		rule := current.valid
		if rule == nil || !rule.applies(request, now) {
			continue
		}

		if matched == nil || rule.key.String() < matched.key.String() {
			matched = rule
		}
	}

	return matched, matched != nil
}

func (c *compiledRule) applies(request target, now time.Time) bool {
	if c.expires != nil && !now.Before(*c.expires) {
		return false
	}

	principals := []string{request.principal}
	if request.email != "" {
		principals = append(principals, request.email)
	}

	return matchesAny(c.principals, principals) &&
		matchesAny(c.groups, request.groups) &&
		matchesAny(c.workspaces, []string{request.workspace}) &&
		matchesAny(c.sessions, []string{request.session}) &&
		(c.attestation == "" || c.attestation == request.attestation) &&
		matchesAny(c.servers, []string{request.server}) &&
		(len(c.tools) == 0 || request.tool != "" && matchesAny(c.tools, request.toolNames()))
}

// toolNames returns the proxied names a tools glob is matched against.
func (t target) toolNames() []string {
	if t.exposedTool != "" {
		return []string{t.tool, t.exposedTool}
	}

	return []string{t.tool}
}

// matchesAny reports whether any value matches any pattern. No patterns
// match anything.
func matchesAny(patterns []glob.Glob, values []string) bool {
	if len(patterns) == 0 {
		return true
	}

	return slices.ContainsFunc(patterns, func(pattern glob.Glob) bool {
		return slices.ContainsFunc(values, pattern.Match)
	})
}

func compile(key types.NamespacedName, rule *v1alpha1.ToolgateRule) (*compiledRule, error) {
	compiled := &compiledRule{key: key, reason: rule.Spec.Reason}

	if rule.Spec.Expires != nil {
		expires := rule.Spec.Expires.Time
		compiled.expires = &expires
	}

	if err := compileMatch(rule.Spec.Match, compiled); err != nil {
		return nil, err
	}

	return compiled, nil
}

// compileMatch compiles the globs of match into compiled. It is the one
// place a ToolgateRule's match and a temporary block's match are compiled,
// so both match the same way.
func compileMatch(match v1alpha1.RuleMatch, compiled *compiledRule) error {
	compiled.attestation = match.Attestation

	var problems []string

	fields := []struct {
		name     string
		patterns []string
		into     *[]glob.Glob
	}{
		{"principals", match.Principals, &compiled.principals},
		{"groups", match.Groups, &compiled.groups},
		{"workspaces", match.Workspaces, &compiled.workspaces},
		{"sessions", match.Sessions, &compiled.sessions},
		{"servers", match.Servers, &compiled.servers},
		{"tools", match.Tools, &compiled.tools},
	}

	for _, field := range fields {
		for index, pattern := range field.patterns {
			// No separators: * matches any run of characters, including /.
			compiledPattern, err := glob.Compile(pattern)
			if err != nil {
				problems = append(problems, fmt.Sprintf("match.%s[%d] is not a valid glob: %v", field.name, index, err))

				continue
			}

			*field.into = append(*field.into, compiledPattern)
		}
	}

	switch match.Attestation {
	case "", attestationUser, attestationAttested:
	default:
		problems = append(problems, fmt.Sprintf("match.attestation %q must be attested or user", match.Attestation))
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}

	return nil
}
