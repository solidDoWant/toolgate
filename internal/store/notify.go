package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Channel is the PostgreSQL notification channel every replica listens on.
const Channel = "toolgate_events"

// RepliesChannel carries a client's answer to a server request from the
// replica that received it to the replica that sent the request. Every
// replica listens on it too.
const RepliesChannel = "toolgate_mcp_replies"

// MaxReplyBytes bounds a payload on RepliesChannel. PostgreSQL refuses a
// notification of 8000 bytes or more.
const MaxReplyBytes = 7 << 10

// ErrReplyTooLarge is returned by NotifyReply for a payload over
// MaxReplyBytes.
var ErrReplyTooLarge = errors.New("reply is larger than 7 KiB")

// Notification kinds.
const (
	KindSession  = "session"
	KindInstance = "instance"
	KindGrant    = "grant"
	KindBlock    = "block"
	KindApproval = "approval"
	// KindScope names a user_scopes row by its ID: the user's defaults,
	// lists or session additions changed.
	KindScope = "scope"
	// KindToolSearch names a template by its UID: the leader rebuilt the
	// template's tool search rows.
	KindToolSearch = "toolsearch"
	// KindMCPSession names an MCP session by its Mcp-Session-Id: the
	// session ended, so every replica closes its copy.
	KindMCPSession = "mcpsession"
)

const (
	subscriberBuffer = 64
	maxBackoff       = 5 * time.Second
)

// Event is one change notification. When Missed is true, Kind and ID are
// empty: notifications may have been lost (the listener reconnected, or the
// subscriber fell behind), and the subscriber must re-read all the state it
// depends on.
type Event struct {
	Kind   string
	ID     string
	Missed bool
}

// Notify queues the notification "<kind>:<id>" on Channel through q. Call it
// inside the transaction that changes the row, so the notification is
// delivered at commit or not at all.
func Notify(ctx context.Context, q db.DBTX, kind, id string) error {
	if err := db.New(q).Notify(ctx, kind+":"+id); err != nil {
		return fmt.Errorf("notify %s:%s: %w", kind, id, err)
	}

	return nil
}

// NotifyReply queues payload on RepliesChannel through q. A payload over
// MaxReplyBytes is refused with ErrReplyTooLarge.
func NotifyReply(ctx context.Context, q db.DBTX, payload []byte) error {
	if len(payload) > MaxReplyBytes {
		return ErrReplyTooLarge
	}

	if err := db.New(q).NotifyReply(ctx, string(payload)); err != nil {
		return fmt.Errorf("notify reply: %w", err)
	}

	return nil
}

// Listener holds one connection that LISTENs on Channel and passes every
// notification to its subscribers. When the connection drops, it
// reconnects and sends every subscriber an Event with Missed set.
type Listener struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu          sync.Mutex
	subscribers map[*subscriber]struct{}
	replies     map[chan []byte]struct{}
	listening   chan struct{}
}

type subscriber struct {
	events chan Event
	// missed is set when an event could not be buffered; the next
	// delivery sends a Missed event first.
	missed bool
}

// NewListener returns a Listener that takes its connection from pool.
func NewListener(pool *pgxpool.Pool, logger *slog.Logger) *Listener {
	return &Listener{
		pool:        pool,
		logger:      logger,
		subscribers: make(map[*subscriber]struct{}),
		replies:     make(map[chan []byte]struct{}),
		listening:   make(chan struct{}),
	}
}

// Subscribe returns a channel of events and a function that ends the
// subscription. Events are buffered; a subscriber that falls behind gets a
// Missed event instead of the ones it could not take.
func (l *Listener) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{events: make(chan Event, subscriberBuffer)}

	l.mu.Lock()
	l.subscribers[sub] = struct{}{}
	l.mu.Unlock()

	var once sync.Once

	return sub.events, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subscribers, sub)
			l.mu.Unlock()
		})
	}
}

// SubscribeReplies returns a channel of the payloads that arrive on
// RepliesChannel and a function that ends the subscription. A payload that
// finds the buffer full is dropped: a lost answer leaves its call waiting
// until the call's own wait ends.
func (l *Listener) SubscribeReplies() (<-chan []byte, func()) {
	replies := make(chan []byte, subscriberBuffer)

	l.mu.Lock()
	l.replies[replies] = struct{}{}
	l.mu.Unlock()

	var once sync.Once

	return replies, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.replies, replies)
			l.mu.Unlock()
		})
	}
}

// Listening is closed once the first LISTEN has succeeded.
func (l *Listener) Listening() <-chan struct{} {
	return l.listening
}

// Run listens until ctx ends, reconnecting with backoff after errors. It
// returns ctx's error.
func (l *Listener) Run(ctx context.Context) error {
	backoff := 100 * time.Millisecond
	connected := false

	for {
		err := l.listen(ctx, connected, func() {
			if !connected {
				connected = true

				close(l.listening)
			}

			backoff = 100 * time.Millisecond
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}

		opsmetrics.NotifyReconnect()
		l.logger.WarnContext(ctx, "PostgreSQL notification listener disconnected; reconnecting", slog.String("error", err.Error()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = min(2*backoff, maxBackoff)
	}
}

// listen runs one connection. After LISTEN succeeds it calls onListening
// and, when reconnected is true, reports a Missed event to subscribers.
func (l *Listener) listen(ctx context.Context, reconnected bool, onListening func()) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}

	// The connection leaves the pool: its session state (LISTEN) must not
	// be shared, and a broken one must not be reused.
	raw := conn.Hijack()
	defer func() { _ = raw.Close(context.WithoutCancel(ctx)) }()

	for _, channel := range []string{Channel, RepliesChannel} {
		if _, err := raw.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return fmt.Errorf("listen on %s: %w", channel, err)
		}
	}

	onListening()

	if reconnected {
		l.broadcast(Event{Missed: true})
	}

	for {
		notification, err := raw.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}

		if notification.Channel == RepliesChannel {
			l.broadcastReply([]byte(notification.Payload))

			continue
		}

		kind, id, ok := strings.Cut(notification.Payload, ":")
		if !ok {
			l.logger.WarnContext(ctx, "ignoring malformed notification", slog.String("channel", notification.Channel))

			continue
		}

		l.broadcast(Event{Kind: kind, ID: id})
	}
}

func (l *Listener) broadcast(event Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for sub := range l.subscribers {
		sub.deliver(event)
	}
}

func (l *Listener) broadcastReply(payload []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for replies := range l.replies {
		select {
		case replies <- payload:
		default:
		}
	}
}

func (s *subscriber) deliver(event Event) {
	if s.missed {
		select {
		case s.events <- Event{Missed: true}:
			s.missed = false
		default:
			return
		}
	}

	select {
	case s.events <- event:
	default:
		s.missed = true
	}
}
