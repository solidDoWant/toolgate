package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSubscriberThatFallsBehindGetsMissed(t *testing.T) {
	sub := &subscriber{events: make(chan Event, 2)}

	sub.deliver(Event{Kind: KindSession, ID: "1"})
	sub.deliver(Event{Kind: KindSession, ID: "2"})
	sub.deliver(Event{Kind: KindSession, ID: "3"}) // dropped: the buffer is full

	assert.Equal(t, Event{Kind: KindSession, ID: "1"}, receive(t, sub))
	assert.Equal(t, Event{Kind: KindSession, ID: "2"}, receive(t, sub))

	sub.deliver(Event{Kind: KindSession, ID: "4"})

	assert.Equal(t, Event{Missed: true}, receive(t, sub), "the dropped event is reported as missed")
	assert.Equal(t, Event{Kind: KindSession, ID: "4"}, receive(t, sub))
}

// receive returns the next buffered event without blocking.
func receive(t *testing.T, sub *subscriber) Event {
	t.Helper()

	select {
	case event := <-sub.events:
		return event
	default:
		t.Fatal("no buffered event")

		return Event{}
	}
}
