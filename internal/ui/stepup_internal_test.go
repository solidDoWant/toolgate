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

			check(t, checkStepUpAuthTime(test.authTime, test.started, now, 5*time.Minute))
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
		{name: "form: exactly 4 minutes", authTime: now.Add(-4 * time.Minute), maxAge: 4 * time.Minute, want: true},
		{name: "form: 4 minutes and 1 second", authTime: now.Add(-4*time.Minute - time.Second), maxAge: 4 * time.Minute, want: false},
		{name: "submit: exactly 5 minutes", authTime: now.Add(-5 * time.Minute), maxAge: 5 * time.Minute, want: true},
		{name: "submit: 5 minutes and 1 second", authTime: now.Add(-5*time.Minute - time.Second), maxAge: 5 * time.Minute, want: false},
		{name: "unknown auth_time", authTime: time.Time{}, maxAge: 5 * time.Minute, want: false},
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
			handler := (&UI{}).RequireStepUp(
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
	webUI := &UI{}
	assert.False(t, webUI.StepUpNeeded(Session{AuthTime: time.Now().Add(-3*time.Minute - 50*time.Second)}))
	assert.True(t, webUI.StepUpNeeded(Session{AuthTime: time.Now().Add(-4*time.Minute - 10*time.Second)}))
	assert.True(t, webUI.StepUpNeeded(Session{}))
}

// The step-up window comes from the options, and the form window is a minute
// shorter, but never under a minute.
func TestStepUpWindows(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		wantSubmit time.Duration
		wantForm   time.Duration
	}{
		{name: "default", wantSubmit: 5 * time.Minute, wantForm: 4 * time.Minute},
		{name: "30 minutes", configured: 30 * time.Minute, wantSubmit: 30 * time.Minute, wantForm: 29 * time.Minute},
		{name: "1 hour", configured: time.Hour, wantSubmit: time.Hour, wantForm: 59 * time.Minute},
		{name: "2 minutes", configured: 2 * time.Minute, wantSubmit: 2 * time.Minute, wantForm: time.Minute},
		{name: "90 seconds", configured: 90 * time.Second, wantSubmit: 90 * time.Second, wantForm: time.Minute},
		{name: "1 minute", configured: time.Minute, wantSubmit: time.Minute, wantForm: time.Minute},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			webUI := &UI{options: Options{StepUpMaxAge: test.configured}}
			assert.Equal(t, test.wantSubmit, webUI.StepUpMaxAge())
			assert.Equal(t, test.wantForm, webUI.StepUpFormMaxAge())
		})
	}
}

// With a 30-minute window, a sign-in 10 minutes old still shows the form and
// lets it be submitted.
func TestLongerStepUpWindowAcceptsAnOlderSignIn(t *testing.T) {
	webUI := &UI{options: Options{StepUpMaxAge: 30 * time.Minute}}
	session := Session{Principal: "alice", AuthTime: time.Now().Add(-10 * time.Minute)}

	assert.False(t, webUI.StepUpNeeded(session))
	assert.True(t, (&UI{}).StepUpNeeded(session), "the default window is shorter")

	ran := false
	handler := webUI.RequireStepUp(
		func(*http.Request) string { return "/ui/grants/new" },
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }),
	)
	request := httptest.NewRequestWithContext(context.WithValue(t.Context(), sessionKey{}, session), http.MethodPost, "/ui/grants", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	assert.True(t, ran)
}

func TestCheckStepUpAuthTimeUsesTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	started := now.Add(-31 * time.Minute)

	require.NoError(t, checkStepUpAuthTime(now.Add(-30*time.Minute), started, now, 30*time.Minute))
	require.Error(t, checkStepUpAuthTime(now.Add(-30*time.Minute-time.Second), started, now, 30*time.Minute))
}

func TestWriteStepUpPrompt(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteStepUpPrompt(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/approvals/42", nil))

	assert.Contains(t, recorder.Body.String(), "Sign in again to continue")
	assert.Contains(t, recorder.Body.String(), `href="/ui/login?return_to=%2Fui%2Fapprovals%2F42&amp;step_up=1"`)
}
