package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Registered workspace keys: a user token in Authorization plus an
// assertion in AttestationHeader, signed by a key pair the user registered
// and approved.
const (
	// WorkspaceKeyAssertionType is the `typ` header of an assertion. It
	// alone routes the attestation header to the key verifier.
	WorkspaceKeyAssertionType = "toolgate-workspace-key+jwt"
	// WorkspaceKeyAudience is the only audience an assertion is for.
	WorkspaceKeyAudience = "toolgate"
	// WorkspaceKeyActive is the status of a key that attests.
	WorkspaceKeyActive = "active"

	// maxAssertionLifetime bounds exp - iat, with no skew.
	maxAssertionLifetime = 300
	// assertionLeeway is the clock skew allowed both ways on exp, iat
	// and nbf.
	assertionLeeway = 30 * time.Second
	// maxAssertionIDLength bounds jti.
	maxAssertionIDLength = 128
	// keyTouchInterval is how often last_used_at is written per key.
	keyTouchInterval = time.Minute
)

// The refusal reasons of a failed key attestation.
const (
	reasonKeysOff               = "attestation workspace keys off"
	reasonAssertionInvalid      = "attestation assertion invalid"
	reasonKeyUnknown            = "attestation key unknown"
	reasonKeyNotApproved        = "attestation key not approved"
	reasonAssertionSignature    = "attestation assertion signature invalid"
	reasonKeyOwnerMismatch      = "attestation key owner mismatch"
	reasonKeyRevoked            = "attestation key revoked"
	reasonAssertionLifetime     = "attestation assertion lifetime too long"
	reasonAssertionAudience     = "attestation assertion audience wrong"
	reasonAssertionExpired      = "attestation assertion expired"
	reasonAssertionIssuedFuture = "attestation assertion issued in the future"
)

// keyIDPattern is a JWK SHA-256 thumbprint, base64url without padding.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// WorkspaceKeyOptions enables attestation with registered workspace keys.
type WorkspaceKeyOptions struct {
	Store             WorkspaceKeyStore
	WorkspaceIDPrefix string
	Now               func() time.Time // nil uses time.Now
}

// WorkspaceKey is an approved key as the verifier reads it.
type WorkspaceKey struct {
	ID        int64
	Principal string
	Username  string
	Name      string
	Status    string // active, replaced or revoked
	PublicKey ed25519.PublicKey
	// LastUsedAt is null until the first attested request.
	LastUsedAt *time.Time
}

// WorkspaceKeyStore reads key rows and records their use. Each call goes
// to the database: the verifier keeps no cache, so an approval or a
// revocation applies on the next request on every replica.
type WorkspaceKeyStore interface {
	WorkspaceKey(ctx context.Context, keyID string) (WorkspaceKey, bool, error)
	HasPendingKeyRequest(ctx context.Context, principal, keyID string) (bool, error)
	TouchWorkspaceKey(ctx context.Context, id int64) error
}

// workspaceKeyVerifier verifies assertions of registered keys.
type workspaceKeyVerifier struct {
	options WorkspaceKeyOptions
	logger  *slog.Logger
}

func (v *workspaceKeyVerifier) now() time.Time {
	if v.options.Now != nil {
		return v.options.Now()
	}

	return time.Now()
}

// attest checks an assertion and binds it to the user. It returns the
// attested identity; the user's identity with a refusal for a failed
// verdict; or errAttestationUnavailable when the store fails before a
// verdict.
func (v *workspaceKeyVerifier) attest(ctx context.Context, assertion string, identity Identity) (Identity, *Refusal, error) {
	refused := func(reason string) (Identity, *Refusal, error) {
		return identity, &Refusal{Reason: reason}, nil
	}

	// Only EdDSA is accepted, so none, HMAC and every other algorithm
	// fail here.
	parsed, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(parsed.Headers) != 1 || !keyIDPattern.MatchString(parsed.Headers[0].KeyID) {
		return refused(reasonAssertionInvalid)
	}

	keyID := parsed.Headers[0].KeyID

	row, found, err := v.options.Store.WorkspaceKey(ctx, keyID)
	if err != nil {
		return identity, nil, errors.Join(errAttestationUnavailable, err)
	}

	if !found {
		pending, err := v.options.Store.HasPendingKeyRequest(ctx, identity.Principal, keyID)
		if err != nil {
			return identity, nil, errors.Join(errAttestationUnavailable, err)
		}

		if pending {
			return refused(reasonKeyNotApproved)
		}

		return refused(reasonKeyUnknown)
	}

	// From here on the claims are signed, so the owner and status verdicts
	// are written only for a client that holds the private key.
	var claims jwt.Claims
	if err := parsed.Claims(row.PublicKey, &claims); err != nil {
		return refused(reasonAssertionSignature)
	}

	if row.Principal != identity.Principal {
		return refused(reasonKeyOwnerMismatch)
	}

	if row.Status != WorkspaceKeyActive {
		return refused(reasonKeyRevoked)
	}

	if claims.IssuedAt == nil || claims.Expiry == nil || len(claims.Audience) == 0 ||
		claims.ID == "" || len(claims.ID) > maxAssertionIDLength {
		return refused(reasonAssertionInvalid)
	}

	if int64(*claims.Expiry)-int64(*claims.IssuedAt) > maxAssertionLifetime {
		return refused(reasonAssertionLifetime)
	}

	now := v.now()

	err = claims.ValidateWithLeeway(jwt.Expected{AnyAudience: jwt.Audience{WorkspaceKeyAudience}, Time: now}, assertionLeeway)

	switch {
	case err == nil:
	case errors.Is(err, jwt.ErrInvalidAudience):
		return refused(reasonAssertionAudience)
	case errors.Is(err, jwt.ErrExpired):
		// Every other check passed, so the client holds an approved key
		// of this user and only has to sign a new assertion. Claude Code
		// runs the headers helper again only after a 401.
		return identity, &Refusal{Reason: reasonAssertionExpired, Outcome: OutcomeChallenged}, nil
	case errors.Is(err, jwt.ErrNotValidYet), errors.Is(err, jwt.ErrIssuedInTheFuture):
		return refused(reasonAssertionIssuedFuture)
	default:
		return refused(reasonAssertionInvalid)
	}

	identity.Attestation = AttestationAttested
	// The prefix is read on each request, so a changed setting applies
	// to existing keys after the restart.
	identity.Workspace = v.options.WorkspaceIDPrefix + "/" + row.Username + "/" + row.Name

	// The store's update repeats this condition, so replicas racing on
	// the same key write at most once a minute.
	if row.LastUsedAt == nil || now.Sub(*row.LastUsedAt) > keyTouchInterval {
		if err := v.options.Store.TouchWorkspaceKey(ctx, row.ID); err != nil {
			v.logger.WarnContext(ctx, "cannot record the use of a workspace key", "keyID", keyID, "error", err)
		}
	}

	return identity, nil, nil
}

// attestUser runs the attestation step for a signed-in user: the header
// checks, then the key verifier for a workspace key assertion and the
// workload verifier for anything else.
func (a *Authenticator) attestUser(ctx context.Context, request *http.Request, identity Identity,
	claims map[string]json.RawMessage,
) (Identity, *Refusal, error) {
	values := request.Header.Values(AttestationHeader)

	switch {
	case len(values) == 0 || (len(values) == 1 && values[0] == ""):
		return identity, nil, nil
	case len(values) > 1 || len(values[0]) > MaxAttestationBytes:
		return identity, &Refusal{Reason: reasonAttestationHeader}, nil
	}

	token := values[0]

	// The full algorithm list, so a key assertion is never mistaken for
	// a workload token and sent to TokenReview.
	if parsed, err := jose.ParseSignedCompact(token, tokenAlgorithms); err == nil && len(parsed.Signatures) == 1 {
		if typ, _ := parsed.Signatures[0].Protected.ExtraHeaders[jose.HeaderType].(string); typ == WorkspaceKeyAssertionType {
			if a.keys == nil {
				return identity, &Refusal{Reason: reasonKeysOff}, nil
			}

			return a.keys.attest(ctx, token, identity)
		}
	}

	if a.attest == nil {
		return identity, &Refusal{Reason: reasonAttestationInvalid}, nil
	}

	return a.attest.attest(ctx, token, identity, claims)
}
