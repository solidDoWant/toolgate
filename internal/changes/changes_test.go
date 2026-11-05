package changes_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/changes"
)

func received(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}

func TestFeed(t *testing.T) {
	t.Run("every subscriber receives one merged signal for several notifies", func(t *testing.T) {
		var feed changes.Feed

		first, _ := feed.Subscribe()
		second, _ := feed.Subscribe()

		feed.Notify()
		feed.Notify()

		assert.True(t, received(first))
		assert.False(t, received(first))
		assert.True(t, received(second))
	})

	t.Run("an ended subscription receives nothing", func(t *testing.T) {
		var feed changes.Feed

		signal, stop := feed.Subscribe()
		stop()
		feed.Notify()

		assert.False(t, received(signal))
	})
}
