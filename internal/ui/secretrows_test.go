package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/secrets"
)

func TestSecretRowsShowRelativeTimesAndHideCreatedWhenEqualToUpdated(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	created := now.Add(-72 * time.Hour)
	updated := now.Add(-time.Hour)

	rows := secretRows(now, []secrets.Info{
		{Name: "never-replaced", CreatedAt: created, UpdatedAt: created},
		{Name: "replaced", CreatedAt: created, UpdatedAt: updated},
	}, map[string][]string{"replaced": {"mail", "github"}})

	require.Len(t, rows, 2)

	assert.False(t, rows[0].ShowCreated)
	assert.Equal(t, "3 d ago", rows[0].Updated.Text)
	assert.Equal(t, "2026-11-04 12:00:00 UTC", rows[0].Updated.UTC)
	assert.Empty(t, rows[0].UsedBy)

	assert.True(t, rows[1].ShowCreated)
	assert.Equal(t, "1 h ago", rows[1].Updated.Text)
	assert.Equal(t, "3 d ago", rows[1].Created.Text)
	assert.Equal(t, []string{"mail", "github"}, rows[1].UsedBy)
}
