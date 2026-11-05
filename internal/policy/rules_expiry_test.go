package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// expiryWait bounds how long a test waits for the signal sent when a rule
// expires. quietWait is how long a test watches for a signal that must not
// come.
const (
	expiryWait = 5 * time.Second
	quietWait  = 100 * time.Millisecond
)

type expiryFixture struct {
	clock   *clocktesting.FakeClock
	rules   *policy.Rules
	signals <-chan struct{}
}

func newExpiryFixture(t *testing.T) *expiryFixture {
	t.Helper()

	fake := clocktesting.NewFakeClock(now)
	rules := policy.NewRulesWithClock(fake)
	signals, unsubscribe := rules.Subscribe()
	t.Cleanup(unsubscribe)

	return &expiryFixture{clock: fake, rules: rules, signals: signals}
}

// set records a rule that blocks github until expires and takes the signal
// the change itself sends.
func (f *expiryFixture) set(t *testing.T, name string, expires time.Time) {
	t.Helper()

	require.NoError(t, f.rules.Set(rule(name, v1alpha1.RuleMatch{Servers: []string{"github"}}, &expires)))
	f.requireSignal(t, "the change")
}

func (f *expiryFixture) delete(t *testing.T, name string) {
	t.Helper()

	f.rules.Delete(types.NamespacedName{Namespace: "rules", Name: name})
	f.requireSignal(t, "the delete")
}

func (f *expiryFixture) requireSignal(t *testing.T, cause string) {
	t.Helper()

	select {
	case <-f.signals:
	case <-time.After(expiryWait):
		require.Failf(t, "no signal", "subscribers got no signal after %s", cause)
	}
}

func (f *expiryFixture) requireQuiet(t *testing.T, when string) {
	t.Helper()

	select {
	case <-f.signals:
		require.Failf(t, "unexpected signal", "subscribers got a signal %s", when)
	case <-time.After(quietWait):
	}
}

func (f *expiryFixture) blocked(t *testing.T) bool {
	t.Helper()

	github := template("github", v1alpha1.Access{Users: []string{"alice-sub"}, Environments: "any"}, nil)
	policyEngine := &policy.Engine{Rules: f.rules, Now: f.clock.Now}

	return !policyEngine.Decide(policy.Request{Identity: laptopUser(), Template: github}).Allowed
}

func TestRuleExpirySignalsSubscribers(t *testing.T) {
	fixture := newExpiryFixture(t)
	fixture.set(t, "temporary", now.Add(time.Minute))
	assert.True(t, fixture.blocked(t))

	fixture.clock.Step(time.Minute - time.Nanosecond)
	fixture.requireQuiet(t, "before the rule expired")

	fixture.clock.Step(time.Nanosecond)
	fixture.requireSignal(t, "the rule expired")
	assert.False(t, fixture.blocked(t), "an expired rule applies to nothing")
}

func TestEachRuleExpirySignalsSubscribers(t *testing.T) {
	fixture := newExpiryFixture(t)
	fixture.set(t, "third", now.Add(3*time.Minute))
	fixture.set(t, "first", now.Add(time.Minute))
	fixture.set(t, "second", now.Add(2*time.Minute))

	for _, name := range []string{"first", "second", "third"} {
		fixture.clock.Step(time.Minute)
		fixture.requireSignal(t, "rule "+name+" expired")
	}

	assert.False(t, fixture.blocked(t))
}

func TestRuleChangedBeforeExpiryMovesTimer(t *testing.T) {
	fixture := newExpiryFixture(t)
	fixture.set(t, "temporary", now.Add(time.Minute))
	fixture.set(t, "temporary", now.Add(5*time.Minute))

	fixture.clock.Step(time.Minute)
	fixture.requireQuiet(t, "at the expires time the rule no longer has")
	assert.True(t, fixture.blocked(t))

	fixture.set(t, "temporary", now.Add(2*time.Minute))

	fixture.clock.Step(time.Minute)
	fixture.requireSignal(t, "the rule reached its earlier expires time")
	assert.False(t, fixture.blocked(t))
}

func TestRuleDeletedBeforeExpiryMovesTimer(t *testing.T) {
	fixture := newExpiryFixture(t)
	fixture.set(t, "first", now.Add(time.Minute))
	fixture.set(t, "second", now.Add(2*time.Minute))
	fixture.delete(t, "first")

	fixture.clock.Step(time.Minute)
	fixture.requireQuiet(t, "at the expires time of the deleted rule")

	fixture.clock.Step(time.Minute)
	fixture.requireSignal(t, "the remaining rule expired")
	assert.False(t, fixture.blocked(t))
}
