package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// DefaultRemoteTimeout bounds each request to a remote server whose
// template sets no remote.timeout.
const DefaultRemoteTimeout = 30 * time.Second

// ErrCredentialRefused is returned when a remote server answers 401 or 403:
// it refused the headers built from the user's secrets.
var ErrCredentialRefused = errors.New("the remote server refused the credential")

// IsRemote reports whether an instance row is a remote server's: it has no
// namespace, because toolgate starts nothing for it.
func IsRemote(instance db.Instance) bool {
	return instance.Namespace == ""
}

// RemoteTimeout returns the per-request timeout of a remote template.
func RemoteTimeout(template *v1alpha1.MCPServerTemplate) time.Duration {
	if remote := template.Spec.Remote; remote != nil && remote.Timeout != nil && remote.Timeout.Duration > 0 {
		return remote.Timeout.Duration
	}

	return DefaultRemoteTimeout
}

// MissingSecretError is a header value naming a secret the user hasn't set.
// Its message tells the user how to set it.
type MissingSecretError struct {
	Name string
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("Secret `%s` isn't set. Run `toolgate secret set %s`.", e.Name, e.Name)
}

// RemoteHeaders fills the variables in a remote template's header values
// for one identity. The values hold user secrets: they go only into the
// upstream requests, never into logs, errors, the status or a row. An error
// names a header or a secret, never a value.
func RemoteHeaders(ctx context.Context, template *v1alpha1.MCPServerTemplate, identity Identity, source SecretSource) (http.Header, map[string]time.Time, error) {
	if template.Spec.Remote == nil {
		return nil, nil, fmt.Errorf("template %s is not a remote server", template.Name)
	}

	renderer := &renderer{ctx: ctx, input: Input{Template: template, Identity: identity, Secrets: source}, secretsUsed: map[string]time.Time{}}
	header := http.Header{}

	names := make([]string, 0, len(template.Spec.Remote.Headers))
	for name := range template.Spec.Remote.Headers {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		value, err := renderer.text(template.Spec.Remote.Headers[name], []string{"remote", "headers", name})
		if err != nil {
			var variable *VariableError
			if errors.As(err, &variable) && errors.Is(err, secrets.ErrNotFound) {
				return nil, nil, &MissingSecretError{Name: strings.TrimPrefix(variable.Variable, "secret:")}
			}

			return nil, nil, err
		}

		if !httpguts.ValidHeaderFieldName(name) {
			return nil, nil, fmt.Errorf("remote.headers: %q is not a valid header name", name)
		}

		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, nil, fmt.Errorf("remote.headers.%s: the filled-in value is not a valid header value", name)
		}

		header.Set(name, value)
	}

	return header, renderer.secretsUsed, nil
}

// RemoteAuth carries the headers of one remote connection and records
// whether the server refused them.
type RemoteAuth struct {
	Header  http.Header
	refused atomic.Bool
}

// Refused reports whether a response to these headers was 401 or 403.
func (a *RemoteAuth) Refused() bool { return a.refused.Load() }

// remoteAuthKey is the context key of the RemoteAuth for the upstream
// connections opened with that context.
type remoteAuthKey struct{}

// WithRemoteAuth returns ctx carrying auth: the MCP tool lister and monitor
// connector send its headers on every request.
func WithRemoteAuth(ctx context.Context, auth *RemoteAuth) context.Context {
	return context.WithValue(ctx, remoteAuthKey{}, auth)
}

func remoteAuthFrom(ctx context.Context) *RemoteAuth {
	auth, _ := ctx.Value(remoteAuthKey{}).(*RemoteAuth)

	return auth
}

// credentialError wraps err with ErrCredentialRefused when auth saw a 401 or
// 403.
func credentialError(err error, auth *RemoteAuth) error {
	if err != nil && auth != nil && auth.Refused() {
		return fmt.Errorf("%w: %w", ErrCredentialRefused, err)
	}

	return err
}

// headerTransport adds a remote connection's headers to each request and
// notes a refusal.
type headerTransport struct {
	base http.RoundTripper
	auth *RemoteAuth
}

// RoundTrip implements http.RoundTripper.
func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range t.auth.Header {
		request.Header[name] = slices.Clone(values)
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	response, err := base.RoundTrip(request)
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		t.auth.refused.Store(true)
	}

	return response, err
}

// withRemoteAuth returns httpClient sending auth's headers, or httpClient
// itself when auth is nil.
func withRemoteAuth(httpClient *http.Client, auth *RemoteAuth) *http.Client {
	if auth == nil {
		return httpClient
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	copied := *httpClient
	copied.Transport = &headerTransport{base: httpClient.Transport, auth: auth}

	return &copied
}

// CredentialRefusedReason is the failure reason of a remote instance, and
// the message of a refused call, when the server refuses the user's
// credential. It names the secret to set again, never a header value.
func CredentialRefusedReason(template *v1alpha1.MCPServerTemplate) string {
	names := SecretNamesInHeaders(template)
	if len(names) == 0 {
		return "The remote server refused the request (401 or 403)."
	}

	return fmt.Sprintf("The remote server refused your credential (401 or 403). Check the secret and set it again with `toolgate secret set %s`.", names[0])
}

// SecretNamesInHeaders returns the names of the secrets a remote template's
// header values use, sorted.
func SecretNamesInHeaders(template *v1alpha1.MCPServerTemplate) []string {
	if template.Spec.Remote == nil {
		return nil
	}

	var names []string

	for _, value := range template.Spec.Remote.Headers {
		for rest := value; ; {
			start := strings.Index(rest, "${secret:")
			if start < 0 {
				break
			}

			rest = rest[start+len("${secret:"):]

			end := strings.IndexByte(rest, '}')
			if end < 0 {
				break
			}

			names = append(names, rest[:end])
			rest = rest[end+1:]
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// RemoteAuth fills a remote instance's headers for the identity the
// instance runs as.
func (s *Starter) RemoteAuth(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) (*RemoteAuth, error) {
	identity, err := s.identity(ctx, instance)
	if err != nil {
		return nil, err
	}

	header, _, err := RemoteHeaders(ctx, template, identity, s.Secrets(identity.PrincipalID))
	if err != nil {
		return nil, err
	}

	return &RemoteAuth{Header: header}, nil
}

// startRemote moves a remote instance from pending to starting. toolgate
// starts nothing for it: it checks that the headers fill in and records the
// secrets they use.
func (s *Starter) startRemote(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) error {
	identity, err := s.identity(ctx, instance)
	if err != nil {
		return err
	}

	_, versions, err := RemoteHeaders(ctx, template, identity, s.Secrets(identity.PrincipalID))
	if err != nil {
		if ctx.Err() != nil {
			return err
		}

		return s.fail(ctx, instance, StatusPending, remoteFailureReason(err))
	}

	if err := s.Store.RecordSecretsUsed(ctx, instance.ID, versions); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	if _, err := s.Store.MarkStarting(ctx, instance.ID, template.Generation, nil, time.Time{}); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return nil
}

// checkRemote connects to a starting remote instance with its owner's
// headers, lists its tools and marks it ready. A refused credential fails
// it at once; any other failure is retried until startTimeout.
func (s *Starter) checkRemote(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) error {
	if s.timedOut(instance, template) {
		return s.fail(ctx, instance, StatusStarting, "The remote server didn't answer MCP within "+
			startTimeout(template).String()+".")
	}

	remote, err := s.RemoteAuth(ctx, instance, template)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}

		return s.fail(ctx, instance, StatusStarting, remoteFailureReason(err))
	}

	listCtx, cancel := context.WithTimeout(WithRemoteAuth(ctx, remote), RemoteTimeout(template))
	defer cancel()

	tools, revision, err := s.ListTools(listCtx, EndpointURL(instance, template))

	switch {
	case errors.Is(err, ErrCredentialRefused):
		return s.fail(ctx, instance, StatusStarting, CredentialRefusedReason(template))
	case errors.Is(err, ErrToolListTooLarge):
		return s.fail(ctx, instance, StatusStarting, toolListTooLargeReason)
	case err != nil:
		s.logger().DebugContext(ctx, "remote server not answering MCP",
			slog.String("instance", instance.ID), slog.String("error", err.Error()))

		return nil
	}

	encoded, reason := CheckCuratedToolList(template, tools)
	if reason != "" {
		return s.fail(ctx, instance, StatusStarting, reason)
	}

	if _, err := s.Store.MarkReady(ctx, instance, encoded, revision); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return nil
}

// remoteFailureReason is the instance reason for headers that didn't fill
// in. Neither message holds a header value.
func remoteFailureReason(err error) string {
	var missing *MissingSecretError
	if errors.As(err, &missing) {
		return missing.Error()
	}

	return "The remote server's headers didn't fill in: " + err.Error()
}
