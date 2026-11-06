package runtime_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// serviceAccountLookalike is a principal in a service account's form.
const serviceAccountLookalike = "system:serviceaccount:team:bot"

func TestSnapshotIdentityTakesKindFromTheSession(t *testing.T) {
	for _, test := range []struct {
		name        string
		session     db.Session
		access      v1alpha1.Access
		wantKind    string
		wantAllowed bool
	}{
		{
			name:        "an attested user with a service-account-shaped principal is judged against users",
			session:     db.Session{Principal: serviceAccountLookalike, Attestation: auth.AttestationAttested, Kind: auth.KindUser},
			access:      v1alpha1.Access{Users: []string{serviceAccountLookalike}},
			wantKind:    auth.KindUser,
			wantAllowed: true,
		},
		{
			name:        "an attested user with a service-account-shaped principal is not judged against service accounts",
			session:     db.Session{Principal: serviceAccountLookalike, Attestation: auth.AttestationAttested, Kind: auth.KindUser},
			access:      v1alpha1.Access{ServiceAccounts: []string{serviceAccountLookalike}},
			wantKind:    auth.KindUser,
			wantAllowed: false,
		},
		{
			name:        "an attested user with a spiffe-shaped principal is judged against users",
			session:     db.Session{Principal: "spiffe://example.com/bot", Attestation: auth.AttestationAttested, Kind: auth.KindUser},
			access:      v1alpha1.Access{SPIFFEIDs: []string{"spiffe://example.com/*"}},
			wantKind:    auth.KindUser,
			wantAllowed: false,
		},
		{
			name:        "a service account session is judged against service accounts",
			session:     db.Session{Principal: serviceAccountLookalike, Attestation: auth.AttestationAttested, Kind: auth.KindServiceAccount},
			access:      v1alpha1.Access{ServiceAccounts: []string{serviceAccountLookalike}},
			wantKind:    auth.KindServiceAccount,
			wantAllowed: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := runtime.SnapshotIdentity(test.session)
			assert.Equal(t, test.wantKind, identity.Kind)
			assert.Equal(t, test.wantAllowed, policy.HasAccess(identity, test.access))
		})
	}
}

func TestRenderIdentityTakesKindFromTheSession(t *testing.T) {
	store := &memoryStore{sessions: map[int64]db.Session{
		7: {ID: 7, Principal: serviceAccountLookalike, Attestation: auth.AttestationAttested, Kind: auth.KindUser},
	}}
	starter := &runtime.Starter{Store: store}

	identity, err := starter.RenderIdentity(t.Context(), db.Instance{ID: instanceID, RenderSession: pgtype.Int8{Int64: 7, Valid: true}})
	require.NoError(t, err)
	assert.Equal(t, auth.KindUser, identity.Kind)
	assert.True(t, identity.Attested)
	assert.Equal(t, serviceAccountLookalike, identity.PrincipalID)
}
