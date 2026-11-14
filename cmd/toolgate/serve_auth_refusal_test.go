package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// auditRows keeps the audit rows written.
type auditRows struct {
	rows []db.InsertAuditEventParams
}

func (s *auditRows) InsertAuditEvent(_ context.Context, row db.InsertAuditEventParams) error {
	s.rows = append(s.rows, row)

	return nil
}

func TestRefusalRecorderWritesTheTrustDomain(t *testing.T) {
	tests := []struct {
		name        string
		refusal     auth.Refusal
		wantDetails map[string]any
	}{
		{
			name:    "SPIFFE refusal",
			refusal: auth.Refusal{Reason: "SPIFFE trust domain not allowed", TrustDomain: "other.test", RemoteAddress: "192.0.2.1"},
			wantDetails: map[string]any{
				"reason": "SPIFFE trust domain not allowed", "trustDomain": "other.test",
				"issuer": "", "audience": []any{}, "remoteAddress": "192.0.2.1",
			},
		},
		{
			name: "challenged attestation",
			refusal: auth.Refusal{
				Principal: "alice-sub", Reason: "attestation assertion expired", Outcome: auth.OutcomeChallenged, RemoteAddress: "192.0.2.1",
			},
			wantDetails: map[string]any{
				"reason": "attestation assertion expired", "outcome": "challenged",
				"issuer": "", "audience": []any{}, "remoteAddress": "192.0.2.1",
			},
		},
		{
			name:    "other refusal",
			refusal: auth.Refusal{Reason: "invalid token", Issuer: "https://idp.example", RemoteAddress: "192.0.2.1"},
			wantDetails: map[string]any{
				"reason": "invalid token", "issuer": "https://idp.example", "audience": []any{}, "remoteAddress": "192.0.2.1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &auditRows{}
			recorder := refusalRecorder{recorder: audit.NewRecorder(io.Discard, store), logger: slog.New(slog.DiscardHandler)}

			recorder.RecordAuthRefusal(t.Context(), test.refusal)

			require.Len(t, store.rows, 1)
			assert.Equal(t, audit.TypeAuthRefuse, store.rows[0].Type)

			var details map[string]any
			require.NoError(t, json.Unmarshal(store.rows[0].Details, &details))
			assert.Equal(t, test.wantDetails, details)
		})
	}
}
