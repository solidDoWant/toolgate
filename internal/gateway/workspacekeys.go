package gateway

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// Workspace key limits and defaults.
const (
	// MaxWorkspaceKeys is the most active workspace keys one user has.
	MaxWorkspaceKeys = 20
	// DefaultWorkspaceKeysPrefix is the first segment of a key's workspace
	// when none is configured.
	DefaultWorkspaceKeysPrefix = "registered"
)

// Audit event types of workspace keys.
const (
	TypeWorkspaceKeyRegister = "workspace.key.register"
	TypeWorkspaceKeyApprove  = "workspace.key.approve"
	TypeWorkspaceKeyReplace  = "workspace.key.replace"
	TypeWorkspaceKeyRevoke   = "workspace.key.revoke"
)

// Statuses of a listed workspace key.
const (
	KeyStatusActive  = "active"
	KeyStatusPending = "pending"
)

const accessKindKey = "key"

// Messages of the workspace key rules, shown to the user.
const (
	MessageWorkspaceKeysOff   = "Workspace keys are off on this gateway."
	messageKeyRegisterNonUser = "Only users can register workspace keys."
	messageKeyListNonUser     = "Only users have workspace keys."
	messageKeyName            = "A key name has 1 to 63 letters, digits, `.`, `_` or `-`."
	messageKeyPublicKey       = "publicKey must be an Ed25519 public key of 32 bytes, base64url without padding."
	messageKeyUsername        = "Your sign-in has no user name that fits in a workspace ID. toolgate needs the preferred_username claim, " +
		"with 1 to 63 letters, digits, `.`, `_` or `-`."
	messageKeyTaken = "This key is already registered. Make a new key with a new --key-file."
)

// keySegmentPattern is a key name, a user name or a prefix: one segment of a
// key's workspace ID.
var keySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// KeyChange is the change JSON of a key access request.
type KeyChange struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	KeyID    string `json:"keyID"`
	// PublicKey is the Ed25519 public key, base64url without padding.
	PublicKey string `json:"publicKey"`
}

// KeyRegistration is the outcome of a registration: the key is active
// already, or Request is its pending key request.
type KeyRegistration struct {
	// Active is true when the same key is active for the name already.
	Active      bool
	Request     string
	Name        string
	Workspace   string
	Fingerprint string
	// Replaces is the fingerprint of the active key the request replaces,
	// or "" when the name has none.
	Replaces string
}

// WorkspaceKeyEntry is one active key or pending key request of a user.
type WorkspaceKeyEntry struct {
	Name        string
	Workspace   string
	Fingerprint string
	// Status is KeyStatusActive or KeyStatusPending.
	Status       string
	RegisteredAt time.Time
	// ApprovedAt and LastUsedAt are nil for a pending request, and
	// LastUsedAt also for a key not used yet.
	ApprovedAt *time.Time
	LastUsedAt *time.Time
	// Request is the pending request's ID, or "" for an active key.
	Request string
}

// WorkspaceKeyList is a user's keys, and whether keys attest on this
// gateway.
type WorkspaceKeyList struct {
	Enabled bool
	Keys    []WorkspaceKeyEntry
}

// KeyRevocation is the outcome of a revocation.
type KeyRevocation struct {
	Revoked        bool
	DeniedRequests int
}

// SetWorkspaceKeys sets whether registering and approving workspace keys is
// on, and the first segment of a key's workspace ID. An empty prefix is
// DefaultWorkspaceKeysPrefix.
func (w *Workspaces) SetWorkspaceKeys(enabled bool, prefix string) {
	if prefix == "" {
		prefix = DefaultWorkspaceKeysPrefix
	}

	w.workspaceKeys, w.workspaceKeysPrefix = enabled, prefix
}

// keyWorkspace is the workspace ID a key gives, with the prefix configured
// now.
func (w *Workspaces) keyWorkspace(username, name string) string {
	prefix := w.workspaceKeysPrefix
	if prefix == "" {
		prefix = DefaultWorkspaceKeysPrefix
	}

	return prefix + "/" + username + "/" + name
}

// KeyFingerprint is the fingerprint people compare for a key ID.
func KeyFingerprint(keyID string) string { return "SHA256:" + keyID }

// workspaceKeyID is the RFC 7638 JWK thumbprint of an Ed25519 public key,
// base64url without padding.
func workspaceKeyID(public ed25519.PublicKey) (string, error) {
	thumbprint, err := (&jose.JSONWebKey{Key: public}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(thumbprint), nil
}

// RegisterKey makes a pending key request for identity's public key under
// name, or returns the request or the active key that already has it.
func (w *Workspaces) RegisterKey(ctx context.Context, identity auth.Identity, name, publicKey string) (KeyRegistration, error) {
	if identity.Kind != auth.KindUser {
		return KeyRegistration{}, workspaceError(http.StatusForbidden, messageKeyRegisterNonUser)
	}

	if !w.workspaceKeys {
		return KeyRegistration{}, workspaceError(http.StatusConflict, MessageWorkspaceKeysOff)
	}

	if !keySegmentPattern.MatchString(name) {
		return KeyRegistration{}, workspaceError(http.StatusBadRequest, messageKeyName)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return KeyRegistration{}, workspaceError(http.StatusBadRequest, messageKeyPublicKey)
	}

	keyID, err := workspaceKeyID(ed25519.PublicKey(decoded))
	if err != nil {
		return KeyRegistration{}, workspaceError(http.StatusBadRequest, messageKeyPublicKey)
	}

	if !keySegmentPattern.MatchString(identity.Username) {
		return KeyRegistration{}, workspaceError(http.StatusBadRequest, messageKeyUsername)
	}

	change := KeyChange{Name: name, Username: identity.Username, KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(decoded)}
	registration := KeyRegistration{Name: name, Workspace: w.keyWorkspace(identity.Username, name), Fingerprint: KeyFingerprint(keyID)}

	err = pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		var err error

		registration, err = w.registerKey(ctx, tx, identity, change, registration)

		return err
	})
	if err != nil {
		return KeyRegistration{}, err
	}

	return registration, nil
}

func (w *Workspaces) registerKey(ctx context.Context, tx pgx.Tx, identity auth.Identity, change KeyChange, registration KeyRegistration) (KeyRegistration, error) {
	queries := db.New(tx)

	if _, err := lockUserScope(ctx, queries, identity.Principal); err != nil {
		return KeyRegistration{}, err
	}

	holder, found, err := activeKeyByKeyID(ctx, queries, change.KeyID)
	if err != nil {
		return KeyRegistration{}, err
	}

	if found {
		if holder.Principal != identity.Principal || holder.Name != change.Name {
			return KeyRegistration{}, workspaceError(http.StatusConflict, messageKeyTaken)
		}

		registration.Active = true
		registration.Workspace = w.keyWorkspace(holder.Username, holder.Name)

		return registration, nil
	}

	current, hasCurrent, err := activeKeyByName(ctx, queries, identity.Principal, change.Name)
	if err != nil {
		return KeyRegistration{}, err
	}

	if hasCurrent {
		registration.Replaces = KeyFingerprint(current.KeyID)
	}

	pending, err := queries.GetPendingKeyRequestOfPrincipal(ctx, db.GetPendingKeyRequestOfPrincipalParams{
		Principal: identity.Principal, Name: change.Name, KeyID: change.KeyID,
	})

	switch {
	case err == nil:
		registration.Request = pending.ID

		return registration, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return KeyRegistration{}, fmt.Errorf("read key request: %w", err)
	}

	if err := w.checkKeyLimit(ctx, queries, identity.Principal, hasCurrent,
		"You already have %d workspace keys. Revoke one first."); err != nil {
		return KeyRegistration{}, err
	}

	request, err := w.insertKeyRequest(ctx, queries, identity, change)
	if err != nil {
		return KeyRegistration{}, err
	}

	registration.Request = request.ID

	var replaces any
	if hasCurrent {
		replaces = registration.Replaces
	}

	recorder := audit.NewRecorder(w.auditOutput, queries)
	if err := recorder.Record(ctx, accessEvent(identity, TypeAccessRequest, request)); err != nil {
		return KeyRegistration{}, err
	}

	if err := recorder.Record(ctx, keyEvent(identity, TypeWorkspaceKeyRegister, registration.Workspace, map[string]any{
		"name": change.Name, "fingerprint": registration.Fingerprint, "request": request.ID, "replaces": replaces,
	})); err != nil {
		return KeyRegistration{}, err
	}

	if err := store.Notify(ctx, tx, store.KindApproval, request.ID); err != nil {
		return KeyRegistration{}, err
	}

	return registration, nil
}

// insertKeyRequest inserts a pending key request within the principal's
// pending limit.
func (w *Workspaces) insertKeyRequest(ctx context.Context, queries *db.Queries, identity auth.Identity, change KeyChange) (db.AccessRequest, error) {
	generated, err := uuid.NewV7()
	if err != nil {
		return db.AccessRequest{}, fmt.Errorf("access request ID: %w", err)
	}

	encoded, err := json.Marshal(change)
	if err != nil {
		return db.AccessRequest{}, fmt.Errorf("encode key change: %w", err)
	}

	if err := queries.LockPrincipalAccessRequests(ctx, identity.Principal); err != nil {
		return db.AccessRequest{}, fmt.Errorf("lock access requests: %w", err)
	}

	request, err := queries.InsertKeyRequestWithinLimit(ctx, db.InsertKeyRequestWithinLimitParams{
		ID: hex.EncodeToString(generated[:]), Principal: identity.Principal, Workspace: identity.Workspace,
		Verified:        VerifiedWorkspace(identity) != "",
		Change:          encoded,
		ApprovalTimeout: pgtype.Interval{Microseconds: testtime.Scale(w.approvalTimeout).Microseconds(), Valid: true},
		PendingLimit:    MaxPendingAccessRequestsPerPrincipal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AccessRequest{}, workspaceError(http.StatusConflict,
			"You already have %d pending access requests. Approve or deny some first.", MaxPendingAccessRequestsPerPrincipal)
	}

	if err != nil {
		return db.AccessRequest{}, fmt.Errorf("insert key request: %w", err)
	}

	return request, nil
}

// checkKeyLimit refuses a new key when the principal has MaxWorkspaceKeys
// active keys and none of them is replaced (hasCurrent).
func (w *Workspaces) checkKeyLimit(ctx context.Context, queries *db.Queries, principal string, hasCurrent bool, message string) error {
	if hasCurrent {
		return nil
	}

	count, err := queries.CountActiveWorkspaceKeysOfPrincipal(ctx, principal)
	if err != nil {
		return fmt.Errorf("count workspace keys: %w", err)
	}

	if count >= MaxWorkspaceKeys {
		return workspaceError(http.StatusConflict, message, MaxWorkspaceKeys)
	}

	return nil
}

// ApproveKeyRequest compare-and-sets identity's pending key request to
// approved and makes its key active, replacing the active key of the same
// name, in tx. The approval page calls it after checking the step-up. Any
// error leaves tx to be rolled back, so the request stays pending; a
// *WorkspaceError says why to the user.
func (w *Workspaces) ApproveKeyRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error {
	if !w.workspaceKeys {
		return workspaceError(http.StatusConflict, MessageWorkspaceKeysOff)
	}

	queries := db.New(tx)

	// The user_scopes lock comes before the compare-and-set, the same order
	// as RevokeKey, so an approval and a revoke never deadlock.
	if _, err := lockUserScope(ctx, queries, identity.Principal); err != nil {
		return err
	}

	request, err := queries.ApproveAccessRequest(ctx, db.ApproveAccessRequestParams{
		ID: id, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("approve access request: %w", err)
	}

	if request.Kind != accessKindKey {
		return ErrAccessRequestUndecidable
	}

	var change KeyChange
	if err := json.Unmarshal(request.Change, &change); err != nil {
		return fmt.Errorf("decode change of key request %s: %w", id, err)
	}

	publicKey, err := base64.RawURLEncoding.DecodeString(change.PublicKey)
	if err != nil {
		return fmt.Errorf("decode public key of key request %s: %w", id, err)
	}

	holder, found, err := activeKeyByKeyID(ctx, queries, change.KeyID)
	if err != nil {
		return err
	}

	if found {
		if holder.Principal != request.Principal || holder.Name != change.Name {
			return workspaceError(http.StatusConflict, messageKeyTaken)
		}

		return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessApprove)
	}

	current, hasCurrent, err := activeKeyByName(ctx, queries, request.Principal, change.Name)
	if err != nil {
		return err
	}

	if err := w.checkKeyLimit(ctx, queries, request.Principal, hasCurrent,
		"You already have %d workspace keys. Revoke one, then approve again."); err != nil {
		return err
	}

	workspace := w.keyWorkspace(change.Username, change.Name)
	fingerprint := KeyFingerprint(change.KeyID)
	recorder := audit.NewRecorder(w.auditOutput, queries)

	if hasCurrent {
		if _, err := queries.ReplaceWorkspaceKey(ctx, db.ReplaceWorkspaceKeyParams{Principal: request.Principal, Name: change.Name}); err != nil {
			return fmt.Errorf("replace workspace key: %w", err)
		}

		if err := recorder.Record(ctx, keyEvent(identity, TypeWorkspaceKeyReplace, workspace, map[string]any{
			"name": change.Name, "oldFingerprint": KeyFingerprint(current.KeyID), "newFingerprint": fingerprint, "request": request.ID,
		})); err != nil {
			return err
		}
	}

	if _, err := queries.InsertWorkspaceKey(ctx, db.InsertWorkspaceKeyParams{
		Principal: request.Principal, Username: change.Username, Name: change.Name, KeyID: change.KeyID,
		PublicKey: publicKey, RequestID: request.ID, RegisteredAt: request.CreatedAt,
	}); err != nil {
		return fmt.Errorf("insert workspace key: %w", err)
	}

	if err := recorder.Record(ctx, keyEvent(identity, TypeWorkspaceKeyApprove, workspace, map[string]any{
		"name": change.Name, "fingerprint": fingerprint, "request": request.ID,
	})); err != nil {
		return err
	}

	return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessApprove)
}

// RevokeKey denies identity's pending key requests for name and revokes its
// active key of that name, in one transaction. via is the
// workspace.key.revoke event's via. It works while workspace keys are off,
// because it only narrows.
func (w *Workspaces) RevokeKey(ctx context.Context, identity auth.Identity, name, via string) (KeyRevocation, error) {
	if identity.Kind != auth.KindUser {
		return KeyRevocation{}, workspaceError(http.StatusForbidden, messageKeyListNonUser)
	}

	var result KeyRevocation

	err := pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := lockUserScope(ctx, queries, identity.Principal); err != nil {
			return err
		}

		denied, err := queries.DenyPendingKeyRequestsByName(ctx, db.DenyPendingKeyRequestsByNameParams{
			Principal: pgtype.Text{String: identity.Principal, Valid: true}, Name: name,
		})
		if err != nil {
			return fmt.Errorf("deny key requests: %w", err)
		}

		for _, request := range denied {
			if err := w.recordAccessDecision(ctx, tx, identity, request, TypeAccessDeny); err != nil {
				return err
			}
		}

		revoked, err := queries.RevokeWorkspaceKey(ctx, db.RevokeWorkspaceKeyParams{Principal: identity.Principal, Name: name})

		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("revoke workspace key: %w", err)
		default:
			result.Revoked = true

			if err := audit.NewRecorder(w.auditOutput, queries).Record(ctx, keyEvent(identity, TypeWorkspaceKeyRevoke,
				w.keyWorkspace(revoked.Username, revoked.Name), map[string]any{
					"name": revoked.Name, "fingerprint": KeyFingerprint(revoked.KeyID), "via": via,
				})); err != nil {
				return err
			}
		}

		result.DeniedRequests = len(denied)

		if !result.Revoked && result.DeniedRequests == 0 {
			return workspaceError(http.StatusNotFound, "You have no key named `%s`.", name)
		}

		return nil
	})
	if err != nil {
		return KeyRevocation{}, err
	}

	return result, nil
}

// WorkspaceKeys lists identity's active keys and pending, unexpired key
// requests, sorted by name and then status.
func (w *Workspaces) WorkspaceKeys(ctx context.Context, identity auth.Identity) (WorkspaceKeyList, error) {
	if identity.Kind != auth.KindUser {
		return WorkspaceKeyList{}, workspaceError(http.StatusForbidden, messageKeyListNonUser)
	}

	queries := db.New(w.database)

	active, err := queries.ListActiveWorkspaceKeysOfPrincipal(ctx, identity.Principal)
	if err != nil {
		return WorkspaceKeyList{}, fmt.Errorf("list workspace keys: %w", err)
	}

	pending, err := queries.ListPendingKeyRequestsOfPrincipal(ctx, identity.Principal)
	if err != nil {
		return WorkspaceKeyList{}, fmt.Errorf("list key requests: %w", err)
	}

	list := WorkspaceKeyList{Enabled: w.workspaceKeys, Keys: make([]WorkspaceKeyEntry, 0, len(active)+len(pending))}

	for _, key := range active {
		approved := key.ApprovedAt.Time.UTC()
		list.Keys = append(list.Keys, WorkspaceKeyEntry{
			Name: key.Name, Workspace: w.keyWorkspace(key.Username, key.Name), Fingerprint: KeyFingerprint(key.KeyID),
			Status: KeyStatusActive, RegisteredAt: key.RegisteredAt.Time.UTC(), ApprovedAt: &approved,
			LastUsedAt: optionalTime(key.LastUsedAt),
		})
	}

	for _, request := range pending {
		var change KeyChange
		if err := json.Unmarshal(request.Change, &change); err != nil {
			return WorkspaceKeyList{}, fmt.Errorf("decode change of key request %s: %w", request.ID, err)
		}

		list.Keys = append(list.Keys, WorkspaceKeyEntry{
			Name: change.Name, Workspace: w.keyWorkspace(change.Username, change.Name), Fingerprint: KeyFingerprint(change.KeyID),
			Status: KeyStatusPending, RegisteredAt: request.CreatedAt.Time.UTC(), Request: request.ID,
		})
	}

	// Active sorts before pending; a stable sort keeps the queries' order
	// within one name and status.
	slices.SortStableFunc(list.Keys, func(a, b WorkspaceKeyEntry) int {
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}

		return strings.Compare(a.Status, b.Status)
	})

	return list, nil
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}

	converted := value.Time.UTC()

	return &converted
}

func activeKeyByKeyID(ctx context.Context, queries *db.Queries, keyID string) (db.WorkspaceKey, bool, error) {
	key, err := queries.GetActiveWorkspaceKeyByKeyID(ctx, keyID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.WorkspaceKey{}, false, nil
	case err != nil:
		return db.WorkspaceKey{}, false, fmt.Errorf("read workspace key: %w", err)
	}

	return key, true, nil
}

func activeKeyByName(ctx context.Context, queries *db.Queries, principal, name string) (db.WorkspaceKey, bool, error) {
	key, err := queries.GetActiveWorkspaceKeyByName(ctx, db.GetActiveWorkspaceKeyByNameParams{Principal: principal, Name: name})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.WorkspaceKey{}, false, nil
	case err != nil:
		return db.WorkspaceKey{}, false, fmt.Errorf("read workspace key: %w", err)
	}

	return key, true, nil
}

// keyEvent is a workspace key event: its principal is the key's owner and
// its workspace the key's workspace.
func keyEvent(identity auth.Identity, eventType, workspace string, details map[string]any) audit.Event {
	return audit.Event{
		Type:        eventType,
		Principal:   identity.Principal,
		Workspace:   workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
		Details:     details,
	}
}
