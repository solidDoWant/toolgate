package secrets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// memoryStore keeps user_secrets rows in memory, with the semantics of the
// SQL queries.
type memoryStore struct {
	mutex sync.Mutex
	rows  map[[2]string]db.UserSecret
}

func newMemoryStore() *memoryStore {
	return &memoryStore{rows: map[[2]string]db.UserSecret{}}
}

func (s *memoryStore) PutUserSecret(_ context.Context, arg db.PutUserSecretParams) (db.UserSecret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	key := [2]string{arg.Principal, arg.Name}

	row, exists := s.rows[key]
	if !exists {
		row = db.UserSecret{Principal: arg.Principal, Name: arg.Name, CreatedAt: now}
	}

	row.Ciphertext, row.KeyID, row.UpdatedAt = arg.Ciphertext, arg.KeyID, now
	s.rows[key] = row

	return row, nil
}

func (s *memoryStore) GetUserSecret(_ context.Context, arg db.GetUserSecretParams) (db.UserSecret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	row, exists := s.rows[[2]string{arg.Principal, arg.Name}]
	if !exists {
		return db.UserSecret{}, pgx.ErrNoRows
	}

	return row, nil
}

func (s *memoryStore) ListUserSecretNames(_ context.Context, principal string) ([]db.ListUserSecretNamesRow, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	rows := []db.ListUserSecretNamesRow{}

	for key, row := range s.rows {
		if key[0] == principal {
			rows = append(rows, db.ListUserSecretNamesRow{Name: row.Name, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
		}
	}

	sort.Slice(rows, func(left, right int) bool { return rows[left].Name < rows[right].Name })

	return rows, nil
}

func (s *memoryStore) DeleteUserSecret(_ context.Context, arg db.DeleteUserSecretParams) (int64, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	key := [2]string{arg.Principal, arg.Name}
	if _, exists := s.rows[key]; !exists {
		return 0, nil
	}

	delete(s.rows, key)

	return 1, nil
}

func (s *memoryStore) ListUserSecretsNotUnderKey(_ context.Context, arg db.ListUserSecretsNotUnderKeyParams) ([]db.UserSecret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	rows := []db.UserSecret{}

	for _, row := range s.rows {
		if row.KeyID != arg.KeyID && len(rows) < int(arg.BatchSize) {
			rows = append(rows, row)
		}
	}

	return rows, nil
}

func (s *memoryStore) RewrapUserSecret(_ context.Context, arg db.RewrapUserSecretParams) (int64, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	key := [2]string{arg.Principal, arg.Name}

	row, exists := s.rows[key]
	if !exists || !bytes.Equal(row.Ciphertext, arg.OldCiphertext) {
		return 0, nil
	}

	row.Ciphertext, row.KeyID = arg.NewCiphertext, arg.NewKeyID
	s.rows[key] = row

	return 1, nil
}

func (s *memoryStore) row(principal, name string) db.UserSecret {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.rows[[2]string{principal, name}]
}

// memoryRecorder keeps audit events.
type memoryRecorder struct {
	mutex  sync.Mutex
	events []audit.Event
}

func (r *memoryRecorder) Record(_ context.Context, event audit.Event) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.events = append(r.events, event)

	return nil
}

func newKeyset(t *testing.T) *keyset.Handle {
	t.Helper()

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	return handle
}

func newCipher(t *testing.T, handle *keyset.Handle) *secrets.Cipher {
	t.Helper()

	cipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	return cipher
}

// writeKeyset writes handle as a JSON keyset file, the form of the mounted
// Kubernetes Secret.
func writeKeyset(t *testing.T, handle *keyset.Handle) string {
	t.Helper()

	var buffer bytes.Buffer
	require.NoError(t, insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buffer)))

	path := filepath.Join(t.TempDir(), "keyset.json")
	require.NoError(t, os.WriteFile(path, buffer.Bytes(), 0o600))

	return path
}

func TestSetStoresTheValueEncrypted(t *testing.T) {
	store := newMemoryStore()
	handle := newKeyset(t)
	cipher, err := secrets.LoadKeyset(writeKeyset(t, handle))
	require.NoError(t, err)

	broker := secrets.NewBroker(store, cipher)
	value := []byte("ghp_plaintext-value-1234")

	_, err = broker.Set(t.Context(), "alice", "github-token", value)
	require.NoError(t, err)

	row := store.row("alice", "github-token")
	assert.NotContains(t, string(row.Ciphertext), string(value), "the row holds no plaintext")
	assert.Equal(t, int64(cipher.PrimaryKeyID()), row.KeyID)

	got, _, err := broker.Get(t.Context(), "alice", "github-token")
	require.NoError(t, err)
	assert.Equal(t, value, got)

	otherKeyset := secrets.NewBroker(store, newCipher(t, newKeyset(t)))
	_, _, err = otherKeyset.Get(t.Context(), "alice", "github-token")
	require.Error(t, err, "another keyset can't decrypt the value")
}

func TestOtherUsersSecretIsNotReadableOrListable(t *testing.T) {
	store := newMemoryStore()
	broker := secrets.NewBroker(store, newCipher(t, newKeyset(t)))

	_, err := broker.Set(t.Context(), "alice", "github-token", []byte("alice-value"))
	require.NoError(t, err)

	_, _, err = broker.Get(t.Context(), "bob", "github-token")
	require.ErrorIs(t, err, secrets.ErrNotFound)

	listed, err := broker.List(t.Context(), "bob")
	require.NoError(t, err)
	assert.Empty(t, listed)

	require.ErrorIs(t, broker.Delete(t.Context(), "bob", "github-token"), secrets.ErrNotFound)

	_, _, err = broker.Get(t.Context(), "alice", "github-token")
	require.NoError(t, err, "bob's delete left alice's secret")

	// A row moved to another principal or name does not decrypt: the
	// ciphertext is bound to both. The new principal and name have the same
	// lengths as the old ones.
	row := store.row("alice", "github-token")

	for _, moved := range [][2]string{{"carol", "github-token"}, {"alice", "github-tokem"}} {
		_, err = store.PutUserSecret(t.Context(), db.PutUserSecretParams{
			Principal: moved[0], Name: moved[1], Ciphertext: row.Ciphertext, KeyID: row.KeyID,
		})
		require.NoError(t, err)

		_, _, err = broker.Get(t.Context(), moved[0], moved[1])
		require.Error(t, err, "%v", moved)
	}
}

func TestSetBoundsNameAndValue(t *testing.T) {
	broker := secrets.NewBroker(newMemoryStore(), newCipher(t, newKeyset(t)))

	tests := []struct {
		name      string
		secret    string
		size      int
		assertErr require.ErrorAssertionFunc
	}{
		{name: "longest name", secret: strings.Repeat("a", 128), size: 1},
		{name: "name too long", secret: strings.Repeat("a", 129), size: 1, assertErr: require.Error},
		{name: "slash in name", secret: "a/b", size: 1, assertErr: require.Error},
		{name: "empty name", secret: "", size: 1, assertErr: require.Error},
		{name: "name starting with a dash", secret: "--help", size: 1, assertErr: require.Error},
		{name: "dash inside a name", secret: "a-b", size: 1},
		{name: "name starting with a dot", secret: ".a", size: 1},
		{name: "largest value", secret: "big", size: secrets.MaxValueBytes},
		{name: "value too large", secret: "big", size: secrets.MaxValueBytes + 1, assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			_, err := broker.Set(t.Context(), "alice", test.secret, make([]byte, test.size))
			test.assertErr(t, err)
		})
	}
}

func TestRewrapReencryptsUnderThePrimaryKey(t *testing.T) {
	store := newMemoryStore()
	handle := newKeyset(t)
	oldCipher := newCipher(t, handle)
	oldBroker := secrets.NewBroker(store, oldCipher)

	values := map[string]string{"alice": "alice-value", "bob": "bob-value"}
	for principal, value := range values {
		_, err := oldBroker.Set(t.Context(), principal, "token", []byte(value))
		require.NoError(t, err)
	}

	// More rows than one batch.
	const extraRows = 250
	for index := range extraRows {
		_, err := oldBroker.Set(t.Context(), "carol", "token-"+strconv.Itoa(index), []byte("carol-value"))
		require.NoError(t, err)
	}

	// A new keyset version: a new primary key, the old key kept.
	manager := keyset.NewManagerFromHandle(handle)
	newKeyID, err := manager.Add(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	require.NoError(t, manager.SetPrimary(newKeyID))
	rotated, err := manager.Handle()
	require.NoError(t, err)

	rotatedBroker := secrets.NewBroker(store, newCipher(t, rotated))

	count, err := rotatedBroker.Rewrap(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2+extraRows, count)

	for principal := range values {
		assert.Equal(t, int64(newKeyID), store.row(principal, "token").KeyID)
	}

	assert.Equal(t, int64(newKeyID), store.row("carol", "token-"+strconv.Itoa(extraRows-1)).KeyID)

	count, err = rotatedBroker.Rewrap(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count, "a second run has nothing to do")

	// The old key can now be removed.
	require.NoError(t, manager.Disable(oldCipher.PrimaryKeyID()))
	require.NoError(t, manager.Delete(oldCipher.PrimaryKeyID()))
	newOnly, err := manager.Handle()
	require.NoError(t, err)
	require.Len(t, newOnly.KeysetInfo().GetKeyInfo(), 1)

	newOnlyBroker := secrets.NewBroker(store, newCipher(t, newOnly))
	for principal, value := range values {
		got, _, err := newOnlyBroker.Get(t.Context(), principal, "token")
		require.NoError(t, err)
		assert.Equal(t, value, string(got))
	}
}

// apiServer serves the API behind the gateway's authentication, with a fake
// IdP. Requests sign in as the principal doRequest is given.
type apiServer struct {
	server   *httptest.Server
	provider *testutil.OIDCProvider
}

func newAPIServer(t *testing.T, broker *secrets.Broker, recorder *memoryRecorder) apiServer {
	t.Helper()

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})

	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audiences: []string{provider.Audience}})
	require.NoError(t, err)

	mux := http.NewServeMux()
	secrets.NewAPI(broker, recorder, slog.New(slog.DiscardHandler)).Register(mux, func(next http.Handler) http.Handler {
		return authenticator.Middleware(next, false)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return apiServer{server: server, provider: provider}
}

// doRequest sends a request signed in as principal; an empty principal sends
// no token.
func (a apiServer) doRequest(t *testing.T, method, path, principal, body string) (int, string) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, a.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)

	if principal != "" {
		request.Header.Set("Authorization", "Bearer "+a.provider.MintAccessToken(t, testutil.TokenClaims{Subject: principal}))
	}

	response, err := a.server.Client().Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, string(data)
}

func TestAPISetListDelete(t *testing.T) {
	store := newMemoryStore()
	broker := secrets.NewBroker(store, newCipher(t, newKeyset(t)))
	recorder := &memoryRecorder{}
	server := newAPIServer(t, broker, recorder)

	const value = "s3cr3t-value-never-returned"

	status, body := server.doRequest(t, http.MethodPut, "/api/v1/secrets/github-token", "alice", value)
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, value)

	stored, _, err := broker.Get(t.Context(), "alice", "github-token")
	require.NoError(t, err)
	assert.Equal(t, value, string(stored))

	status, body = server.doRequest(t, http.MethodGet, "/api/v1/secrets", "alice", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, value)

	var list secrets.SecretList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Secrets, 1)
	assert.Equal(t, "github-token", list.Secrets[0].Name)
	assert.False(t, list.Secrets[0].CreatedAt.IsZero())
	assert.False(t, list.Secrets[0].UpdatedAt.IsZero())

	var fields struct{ Secrets []map[string]any }
	require.NoError(t, json.Unmarshal([]byte(body), &fields))
	assert.ElementsMatch(t, []string{"name", "createdAt", "updatedAt"}, keys(fields.Secrets[0]), "names and dates only")

	status, body = server.doRequest(t, http.MethodGet, "/api/v1/secrets", "bob", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `{"secrets":[]}`, body, "bob doesn't see alice's secret")

	status, _ = server.doRequest(t, http.MethodDelete, "/api/v1/secrets/github-token", "bob", "")
	assert.Equal(t, http.StatusNotFound, status, "bob can't delete alice's secret")

	status, _ = server.doRequest(t, http.MethodDelete, "/api/v1/secrets/github-token", "alice", "")
	assert.Equal(t, http.StatusNoContent, status)

	_, _, err = broker.Get(t.Context(), "alice", "github-token")
	require.ErrorIs(t, err, secrets.ErrNotFound)

	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()

	require.Len(t, recorder.events, 2)
	assert.Equal(t, audit.TypeSecretSet, recorder.events[0].Type)
	assert.Equal(t, audit.TypeSecretDelete, recorder.events[1].Type)

	for _, event := range recorder.events {
		assert.Equal(t, "alice", event.Principal)
		assert.Equal(t, map[string]any{"secret": "github-token"}, event.Details)
	}
}

func keys(object map[string]any) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}

	return names
}

func TestAPIRefusesUnauthenticatedRequests(t *testing.T) {
	broker := secrets.NewBroker(newMemoryStore(), newCipher(t, newKeyset(t)))
	server := newAPIServer(t, broker, &memoryRecorder{})

	for _, request := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/secrets/github-token"},
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodDelete, "/api/v1/secrets/github-token"},
	} {
		status, _ := server.doRequest(t, request.method, request.path, "", "value")
		assert.Equal(t, http.StatusUnauthorized, status, "%s %s", request.method, request.path)
	}

	listed, err := broker.List(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, listed, "nothing was stored")
}

func TestAPIRefusesInvalidRequests(t *testing.T) {
	server := newAPIServer(t, secrets.NewBroker(newMemoryStore(), newCipher(t, newKeyset(t))), &memoryRecorder{})

	status, _ := server.doRequest(t, http.MethodPut, "/api/v1/secrets/"+strings.Repeat("a", 129), "alice", "value")
	assert.Equal(t, http.StatusBadRequest, status)

	status, _ = server.doRequest(t, http.MethodPut, "/api/v1/secrets/big", "alice", strings.Repeat("a", secrets.MaxValueBytes+1))
	assert.Equal(t, http.StatusRequestEntityTooLarge, status)
}
