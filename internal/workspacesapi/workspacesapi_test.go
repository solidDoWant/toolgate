package workspacesapi_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

func TestChangeUnverified(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      workspacesapi.Unverified
		assertion require.ErrorAssertionFunc
	}{
		{name: "[AC-4-11] true is the unverified allow list", body: `{"action":"allow","unverified":true}`, want: workspacesapi.Unverified{List: true}},
		{name: "[AC-4-11] a string is a default", body: `{"action":"defaults","unverified":"list"}`, want: workspacesapi.Unverified{Default: "list"}},
		{name: "absent", body: `{"action":"allow"}`},
		{name: "a number is refused", body: `{"action":"allow","unverified":1}`, assertion: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertion == nil {
				test.assertion = require.NoError
			}

			var change workspacesapi.Change

			err := json.Unmarshal([]byte(test.body), &change)
			test.assertion(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.want, change.Unverified)

			encoded, err := json.Marshal(change)
			require.NoError(t, err)
			assert.JSONEq(t, test.body, string(encoded))
		})
	}
}
