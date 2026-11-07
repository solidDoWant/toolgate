package ui

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"
)

// Step-up bounds.
const (
	// StepUpMaxAge is how old a session's sign-in may be when a grant is
	// created or a call approved.
	StepUpMaxAge = 5 * time.Minute
	// StepUpFormMaxAge is how old the sign-in may be for a form page to show
	// its form. The extra minute up to StepUpMaxAge leaves time to submit.
	StepUpFormMaxAge = 4 * time.Minute
	// StepUpClockSkew is the clock skew allowed both ways on a step-up's
	// auth_time.
	StepUpClockSkew = 60 * time.Second
)

// Messages of a failed step-up sign-in.
const (
	missingAuthTimeMessage = "Your identity provider didn't say when you signed in, so toolgate can't confirm a fresh sign-in. " +
		"Ask an admin to make it send `auth_time`."
	differentUserMessage = "You signed in as a different user."
)

// checkStepUpAuthTime checks the auth_time of a step-up's ID token against
// the time now and the time the sign-in started: at most StepUpMaxAge old,
// at most StepUpClockSkew in the future, and no earlier than StepUpClockSkew
// before the sign-in started. A zero authTime means the IdP left it out.
func checkStepUpAuthTime(authTime, loginCreated, now time.Time) error {
	switch {
	case authTime.IsZero():
		return errors.New("the ID token has no auth_time")
	case authTime.After(now.Add(StepUpClockSkew)):
		return errors.New("the ID token's auth_time is in the future")
	case now.Sub(authTime) > StepUpMaxAge:
		return errors.New("the ID token's auth_time is more than 5 minutes old")
	case authTime.Before(loginCreated.Add(-StepUpClockSkew)):
		return errors.New("the ID token's auth_time is before the sign-in started")
	default:
		return nil
	}
}

// freshFor reports whether the session's sign-in is at most maxAge old. A
// session whose IdP did not send auth_time is never fresh.
func (s Session) freshFor(maxAge time.Duration, now time.Time) bool {
	return !s.AuthTime.IsZero() && now.Sub(s.AuthTime) <= maxAge
}

// StepUpNeeded reports whether a form page that needs a step-up must show
// the "Sign in again to continue" button instead of its form: the session's
// sign-in is older than StepUpFormMaxAge, or its time is unknown.
func StepUpNeeded(session Session) bool {
	return !session.freshFor(StepUpFormMaxAge, time.Now())
}

// StepUpURL is the step-up sign-in that returns to returnTo.
func StepUpURL(returnTo string) string {
	return LoginPath + "?" + url.Values{"step_up": {"1"}, "return_to": {ReturnTo(returnTo)}}.Encode()
}

var stepUpPage = template.Must(template.New("step-up").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Sign in again - toolgate</title></head>
<body>
<main>
<p>This action needs a fresh sign-in.</p>
<a class="button" href="{{.}}">Sign in again to continue</a>
</main>
</body>
</html>
`))

// WriteStepUpPrompt writes the page a form page shows instead of its form
// when StepUpNeeded: a "Sign in again to continue" button to a step-up that
// returns to the page.
func WriteStepUpPrompt(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_ = stepUpPage.Execute(writer, StepUpURL(request.URL.RequestURI()))
}

// RequireStepUp wraps the POST handler of a form that needs a step-up. When
// the session's sign-in is older than StepUpMaxAge, or its time is unknown,
// next does not run: the request changes nothing and is redirected to a
// step-up that returns to the form at formPath(request). Register the result
// with Handle at the User or Admin level.
func RequireStepUp(formPath func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session, ok := SessionFrom(request.Context())
		if !ok || !session.freshFor(StepUpMaxAge, time.Now()) {
			writer.Header().Set("Cache-Control", "no-store")
			http.Redirect(writer, request, StepUpURL(formPath(request)), http.StatusSeeOther)

			return
		}

		next.ServeHTTP(writer, request)
	})
}
