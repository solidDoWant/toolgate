package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// expiringStore holds a number of expired rows and records each delete.
type expiringStore struct {
	expired int64
	calls   []db.DeleteAuditEventsBeforeParams
}

func (s *expiringStore) DeleteAuditEventsBefore(_ context.Context, arg db.DeleteAuditEventsBeforeParams) (int64, error) {
	s.calls = append(s.calls, arg)
	deleted := min(s.expired, int64(arg.BatchSize))
	s.expired -= deleted

	return deleted, nil
}

func TestRetentionDeletesInBatches(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		expired   int64
		wantCalls int
	}{
		{name: "none", expired: 0, wantCalls: 1},
		{name: "fewer than a batch", expired: 1, wantCalls: 1},
		{name: "exactly one batch", expired: 2, wantCalls: 2},
		{name: "one more than a batch", expired: 3, wantCalls: 2},
		{name: "several batches", expired: 5, wantCalls: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &expiringStore{expired: test.expired}
			retention := audit.NewRetention(store, 24*time.Hour, nil)
			retention.BatchSize = 2
			retention.Now = func() time.Time { return now }

			deleted, err := retention.DeleteExpired(t.Context())
			require.NoError(t, err)
			assert.Equal(t, test.expired, deleted)
			assert.Zero(t, store.expired)
			require.Len(t, store.calls, test.wantCalls)

			for _, call := range store.calls {
				assert.Equal(t, int32(2), call.BatchSize)
				assert.True(t, call.Cutoff.Valid)
				assert.True(t, now.Add(-24*time.Hour).Equal(call.Cutoff.Time))
			}
		})
	}
}

func TestNewRetentionDefaultsTo90Days(t *testing.T) {
	retention := audit.NewRetention(&expiringStore{}, 0, nil)
	assert.Equal(t, 90*24*time.Hour, retention.Age)
}
