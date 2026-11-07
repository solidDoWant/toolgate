package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckStepUpAuthTime(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Minute)

	tests := []struct {
		name     string
		authTime time.Time
		started  time.Time
		check    require.ErrorAssertionFunc
	}{
		{name: "now", authTime: now, started: started},
		{name: "missing", authTime: time.Time{}, started: started, check: require.Error},
		{name: "exactly 5 minutes old", authTime: now.Add(-5 * time.Minute), started: now.Add(-5 * time.Minute)},
		{name: "5 minutes and 1 second old", authTime: now.Add(-5*time.Minute - time.Second), started: now.Add(-5 * time.Minute), check: require.Error},
		{name: "exactly 60 s in the future", authTime: now.Add(60 * time.Second), started: started},
		{name: "61 s in the future", authTime: now.Add(61 * time.Second), started: started, check: require.Error},
		{name: "exactly 60 s before the sign-in started", authTime: started.Add(-60 * time.Second), started: started},
		{name: "61 s before the sign-in started", authTime: started.Add(-61 * time.Second), started: started, check: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := test.check
			if check == nil {
				check = require.NoError
			}

			check(t, checkStepUpAuthTime(test.authTime, test.started, now))
		})
	}
}

func TestSessionFreshFor(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		authTime time.Time
		maxAge   time.Duration
		want     bool
	}{
		{name: "form: exactly 4 minutes", authTime: now.Add(-4 * time.Minute), maxAge: StepUpFormMaxAge, want: true},
		{name: "form: 4 minutes and 1 second", authTime: now.Add(-4*time.Minute - time.Second), maxAge: StepUpFormMaxAge, want: false},
		{name: "submit: exactly 5 minutes", authTime: now.Add(-5 * time.Minute), maxAge: StepUpMaxAge, want: true},
		{name: "submit: 5 minutes and 1 second", authTime: now.Add(-5*time.Minute - time.Second), maxAge: StepUpMaxAge, want: false},
		{name: "unknown auth_time", authTime: time.Time{}, maxAge: StepUpMaxAge, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, Session{AuthTime: test.authTime}.freshFor(test.maxAge, now))
		})
	}
}

// A POST whose sign-in is older than 5 minutes changes
// nothing and redirects to a step-up that returns to the form.
func TestRequireStepUp(t *testing.T) {
	tests := []struct {
		name     string
		authTime time.Time
		wantRun  bool
	}{
		{name: "fresh", authTime: time.Now().Add(-4*time.Minute - 50*time.Second), wantRun: true},
		{name: "older than 5 minutes", authTime: time.Now().Add(-5*time.Minute - 10*time.Second)},
		{name: "unknown auth_time", authTime: time.Time{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ran := false
			handler := RequireStepUp(
				func(*http.Request) string { return "/ui/grants/new?server=github" },
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }),
			)

			request := httptest.NewRequestWithContext(
				context.WithValue(t.Context(), sessionKey{}, Session{Principal: "alice", AuthTime: test.authTime}),
				http.MethodPost, "/ui/grants", nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			assert.Equal(t, test.wantRun, ran)

			if test.wantRun {
				return
			}

			require.Equal(t, http.StatusSeeOther, recorder.Code)

			target, err := url.Parse(recorder.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, LoginPath, target.Path)
			assert.Equal(t, "1", target.Query().Get("step_up"))
			assert.Equal(t, "/ui/grants/new?server=github", target.Query().Get("return_to"))
		})
	}
}

func TestStepUpNeeded(t *testing.T) {
	assert.False(t, StepUpNeeded(Session{AuthTime: time.Now().Add(-3*time.Minute - 50*time.Second)}))
	assert.True(t, StepUpNeeded(Session{AuthTime: time.Now().Add(-4*time.Minute - 10*time.Second)}))
	assert.True(t, StepUpNeeded(Session{}))
}

func TestWriteStepUpPrompt(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteStepUpPrompt(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/approvals/42", nil))

	assert.Contains(t, recorder.Body.String(), "Sign in again to continue")
	assert.Contains(t, recorder.Body.String(), `href="/ui/login?return_to=%2Fui%2Fapprovals%2F42&amp;step_up=1"`)
}
