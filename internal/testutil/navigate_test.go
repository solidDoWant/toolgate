package testutil

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryNetworkChanged(t *testing.T) {
	errChanged := errors.New("page load error " + networkChangedError)
	errOther := errors.New("page load error net::ERR_CONNECTION_REFUSED")

	tests := []struct {
		name          string
		results       []error
		wantCalls     int
		wantErr       error
		errorAssertFn require.ErrorAssertionFunc
	}{
		{name: "success first time", results: []error{nil}, wantCalls: 1},
		{name: "retried then succeeds", results: []error{errChanged, errChanged, nil}, wantCalls: 3},
		{name: "other error not retried", results: []error{errOther, nil}, wantCalls: 1, wantErr: errOther, errorAssertFn: require.Error},
		{name: "other error after a retry stops", results: []error{errChanged, errOther, nil}, wantCalls: 2, wantErr: errOther, errorAssertFn: require.Error},
		{name: "gives up after 3 attempts", results: []error{errChanged, errChanged, errChanged, nil}, wantCalls: 3, wantErr: errChanged, errorAssertFn: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := retryNetworkChanged(t.Context(), 3, time.Millisecond, func() error {
				result := test.results[calls]
				calls++

				return result
			})

			if test.errorAssertFn == nil {
				test.errorAssertFn = require.NoError
			}

			test.errorAssertFn(t, err)
			assert.Equal(t, test.wantCalls, calls)
			assert.Equal(t, test.wantErr, err)
		})
	}
}
