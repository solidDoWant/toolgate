//go:build integration

package gateway_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

// newStartCountingFixture returns a fixture whose ToolSearchSessionStarted
// hook counts its calls.
func newStartCountingFixture(t *testing.T) (*mcpFixture, *atomic.Int32) {
	t.Helper()

	var starts atomic.Int32

	fixture := newMCPFixture(t, func(options *gateway.MCPOptions) {
		options.ToolSearchSessionStarted = func(context.Context) { starts.Add(1) }
	})

	return fixture, &starts
}

func TestToolSearchSessionStartReportsDemand(t *testing.T) {
	t.Run("a new session with tool search on reports its start once, and its ordinary requests report nothing", func(t *testing.T) {
		fixture, starts := newStartCountingFixture(t)

		session := fixture.connect(t, searching(alice()))
		assert.EqualValues(t, 1, starts.Load())

		for range 3 {
			_, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
		}

		assert.EqualValues(t, 1, starts.Load(), "tools/list on a live session isn't demand")
	})

	t.Run("a session without tool search reports nothing", func(t *testing.T) {
		fixture, starts := newStartCountingFixture(t)

		session := fixture.connect(t, alice())
		_, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		assert.Zero(t, starts.Load())
	})

	t.Run("a resumed session with tool search on reports its start again", func(t *testing.T) {
		fixture, starts := newStartCountingFixture(t)

		first := fixture.connect(t, searching(alice()))
		require.NoError(t, first.Close())
		require.EqualValues(t, 1, starts.Load())

		fixture.endSession(t)

		fixture.connect(t, searching(alice()))
		assert.EqualValues(t, 2, starts.Load())
	})
}
