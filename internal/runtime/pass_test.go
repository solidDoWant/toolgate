package runtime_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestAfterLoadedRunsThePassOnlyOnceLoaded(t *testing.T) {
	loaded := make(chan struct{})
	calls := 0
	pass := runtime.AfterLoaded(loaded, func(context.Context) error {
		calls++

		return nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, pass(ctx), context.Canceled)
	assert.Zero(t, calls, "the pass must not run before the catalog is loaded")

	close(loaded)

	require.NoError(t, pass(t.Context()))
	assert.Equal(t, 1, calls)
}
