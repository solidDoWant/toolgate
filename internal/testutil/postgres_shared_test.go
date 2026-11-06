package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaimDatabaseNameRefusesADuplicate(t *testing.T) {
	name, err := newDatabaseName()
	assert.NoError(t, err)

	assert.True(t, claimDatabaseName(name), "first claim")
	assert.False(t, claimDatabaseName(name), "second claim of the same name")

	other, err := newDatabaseName()
	assert.NoError(t, err)
	assert.True(t, claimDatabaseName(other))
}
