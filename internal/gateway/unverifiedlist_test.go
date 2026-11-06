package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

// widensOnPhase3 is how a 0.3 replica decides that a change of the stored
// unverified default widens: only a move from none to all.
func widensOnPhase3(oldStored, newStored string) bool {
	return oldStored == "none" && newStored == "all"
}

func TestUnverifiedListStoredForm(t *testing.T) {
	t.Run("row 241: list is stored as none with unverified_list, so a move to all widens under 0.3's rule", func(t *testing.T) {
		stored, list := gateway.StoreUnverifiedDefault("list")
		assert.Equal(t, "none", stored)
		assert.True(t, list)
		assert.True(t, widensOnPhase3(stored, "all"))
		assert.True(t, gateway.DefaultsChangeWidens("none", "none", stored, list, "all", 1))
	})

	t.Run("row 241: a 0.3 write of none leaves unverified_list, so list stays list", func(t *testing.T) {
		_, list := gateway.StoreUnverifiedDefault("list")
		// A 0.3 write of none sets only unverified_default, so list keeps
		// its value.
		assert.Equal(t, "list", gateway.ReadUnverifiedDefault("none", list))
	})

	t.Run("a write of none or all clears unverified_list", func(t *testing.T) {
		for _, value := range []string{"none", "all"} {
			stored, list := gateway.StoreUnverifiedDefault(value)
			assert.Equal(t, value, stored)
			assert.False(t, list)
			assert.Equal(t, value, gateway.ReadUnverifiedDefault(stored, list))
		}

		assert.Equal(t, "all", gateway.ReadUnverifiedDefault("all", true), "list needs the stored none")
	})

	tests := []struct {
		name                         string
		oldVerified, newVerified     string
		oldUnverified, newUnverified string
		servers                      int
		widens                       bool
	}{
		{name: "none to list with servers", oldVerified: "none", newVerified: "none", oldUnverified: "none", newUnverified: "list", servers: 1, widens: true},
		{name: "none to list with no servers", oldVerified: "none", newVerified: "none", oldUnverified: "none", newUnverified: "list", servers: 0},
		{name: "list to list", oldVerified: "none", newVerified: "none", oldUnverified: "list", newUnverified: "list", servers: 3},
		{name: "list to all", oldVerified: "none", newVerified: "none", oldUnverified: "list", newUnverified: "all", servers: 3, widens: true},
		{name: "all to list", oldVerified: "none", newVerified: "none", oldUnverified: "all", newUnverified: "list", servers: 3},
		{name: "list to none", oldVerified: "none", newVerified: "none", oldUnverified: "list", newUnverified: "none", servers: 3},
		{name: "verified none to all", oldVerified: "none", newVerified: "all", oldUnverified: "none", newUnverified: "none", widens: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored, list := gateway.StoreUnverifiedDefault(test.oldUnverified)
			assert.Equal(t, test.widens,
				gateway.DefaultsChangeWidens(test.oldVerified, test.newVerified, stored, list, test.newUnverified, test.servers))
		})
	}
}
