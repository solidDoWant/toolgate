package auth

import (
	"context"
	"errors"
	"log/slog"
)

// redacted is what every printed form of a SubjectToken shows.
const redacted = "[redacted]"

// SubjectToken is the verified bearer token of a user's request, kept for a
// token exchange. It never prints: format verbs, slog attributes and JSON
// encoding all hide it, so it can't reach a log, a row or an audit event by
// accident. Only Reveal returns the token.
type SubjectToken string

// String hides the token from %v, %s and %+v.
func (SubjectToken) String() string { return redacted }

// GoString hides the token from %#v.
func (SubjectToken) GoString() string { return redacted }

// LogValue hides the token from slog.
func (SubjectToken) LogValue() slog.Value { return slog.StringValue(redacted) }

// errSubjectTokenJSON is the error of every JSON encoding of a SubjectToken.
var errSubjectTokenJSON = errors.New("a subject token can't be encoded as JSON")

// MarshalJSON refuses, so the token never lands in a JSON row or event.
func (SubjectToken) MarshalJSON() ([]byte, error) { return nil, errSubjectTokenJSON }

// Reveal returns the token itself, for building an exchange request.
func (t SubjectToken) Reveal() string { return string(t) }

type subjectTokenKey struct{}

// WithSubjectToken returns a copy of ctx holding token, as Middleware stores
// it for a user's request.
func WithSubjectToken(ctx context.Context, token SubjectToken) context.Context {
	return context.WithValue(ctx, subjectTokenKey{}, token)
}

// SubjectTokenFrom returns the subject token Middleware stored in ctx. A
// request of a workload identity has none.
func SubjectTokenFrom(ctx context.Context) (SubjectToken, bool) {
	token, ok := ctx.Value(subjectTokenKey{}).(SubjectToken)

	return token, ok && token != ""
}
