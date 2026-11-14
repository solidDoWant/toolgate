package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

const (
	// DefaultUpstreamSweepInterval is how often the sweep runs without a
	// notification.
	DefaultUpstreamSweepInterval = 30 * time.Second
	// upstreamDeleteTimeout bounds one DELETE.
	upstreamDeleteTimeout = 5 * time.Second
	// upstreamDeleteConcurrency is how many DELETEs run at once.
	upstreamDeleteConcurrency = 8
)

// UpstreamSessionSweeper ends the upstream sessions of invalid pairs, sends
// a best-effort DELETE for each ended one and removes its row. Only the
// Lease holder runs it. It needs no fencing beyond the Lease: a former
// leader at worst sends a second DELETE, which gets a 404, and deletes rows
// that are already gone.
type UpstreamSessionSweeper struct {
	Pool *pgxpool.Pool
	// Template returns the template an instance was started from.
	Template func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool)
	// RemoteAuth fills a remote instance's headers for its owner
	// (Starter.RemoteAuth). Nil means no DELETE is sent to remote instances.
	RemoteAuth func(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) (*RemoteAuth, error)
	// Subscribe returns the PostgreSQL notifications and a function that
	// ends the subscription (store.Listener.Subscribe). Nil means the sweep
	// runs on its interval only.
	Subscribe func() (<-chan store.Event, func())
	// Interval between runs. Zero means DefaultUpstreamSweepInterval.
	Interval time.Duration
	Log      *slog.Logger
}

// NeedLeaderElection makes the manager run the sweep only on the Lease
// holder.
func (s *UpstreamSessionSweeper) NeedLeaderElection() bool { return true }

// Run sweeps when it starts, every Interval, and after a `session:` or
// `instance:` notification or missed notifications, until ctx ends.
func (s *UpstreamSessionSweeper) Run(ctx context.Context) error {
	var events <-chan store.Event

	if s.Subscribe != nil {
		var unsubscribe func()

		events, unsubscribe = s.Subscribe()
		defer unsubscribe()
	}

	interval := s.Interval
	if interval <= 0 {
		interval = DefaultUpstreamSweepInterval
	}

	ticker := time.NewTicker(testtime.Scale(interval))
	defer ticker.Stop()

	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.logger().WarnContext(ctx, "upstream session sweep failed", slog.String("error", err.Error()))
		}

		for wait := true; wait; {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				wait = false
			case notification := <-events:
				wait = !notification.Missed && notification.Kind != store.KindSession && notification.Kind != store.KindInstance
			}
		}
	}
}

// Sweep does one run: it ends the rows of invalid pairs, sends DELETE for up
// to 256 ended rows, oldest first, and removes them.
func (s *UpstreamSessionSweeper) Sweep(ctx context.Context) error {
	queries := db.New(s.Pool)

	if _, err := queries.EndInvalidUpstreamSessions(ctx); err != nil {
		return fmt.Errorf("end invalid upstream sessions: %w", err)
	}

	rows, err := queries.ListEndedUpstreamSessions(ctx)
	if err != nil {
		return fmt.Errorf("list ended upstream sessions: %w", err)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		problems []error
		slots    = make(chan struct{}, upstreamDeleteConcurrency)
	)

	for _, row := range rows {
		slots <- struct{}{}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			if err := s.finish(ctx, queries, row); err != nil {
				mu.Lock()

				problems = append(problems, err)

				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	return errors.Join(problems...)
}

// finish sends the DELETE for one ended row, when one is due, and removes
// the row.
func (s *UpstreamSessionSweeper) finish(ctx context.Context, queries *db.Queries, row db.ListEndedUpstreamSessionsRow) error {
	if row.InstanceLive || row.InstanceRemote {
		s.sendDelete(ctx, queries, row)
	}

	if ctx.Err() != nil {
		return nil
	}

	if err := queries.DeleteUpstreamSession(ctx, row.ID); err != nil {
		return fmt.Errorf("delete upstream session row: %w", err)
	}

	return nil
}

// sendDelete ends the upstream session with a DELETE from a copy made with
// Adopt. It sends nothing when the instance, its template or a remote
// instance's headers can't be found.
func (s *UpstreamSessionSweeper) sendDelete(ctx context.Context, queries *db.Queries, row db.ListEndedUpstreamSessionsRow) {
	instance, err := queries.GetInstance(ctx, row.InstanceID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger().DebugContext(ctx, "upstream session DELETE skipped", slog.String("instance", row.InstanceID),
				slog.String("error", err.Error()))
		}

		return
	}

	if s.Template == nil {
		return
	}

	template, found := s.Template(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found || string(template.UID) != instance.TemplateUid {
		return
	}

	revision := instance.McpRevision
	if revision == "" {
		revision = RevisionStateful
	}

	if revision >= RevisionStateless {
		return
	}

	options := UpstreamOptions{
		Adopt:         &AdoptedSession{ID: row.UpstreamSessionID, InitializeResult: json.RawMessage(row.InitializeResult)},
		DeleteOnClose: &atomic.Bool{},
		HTTPClient:    &http.Client{Timeout: testtime.Scale(upstreamDeleteTimeout)},
	}
	options.DeleteOnClose.Store(true)

	if template.Spec.Remote != nil {
		if s.RemoteAuth == nil {
			return
		}

		auth, err := s.RemoteAuth(ctx, instance, template)
		if err != nil || auth == nil {
			s.logger().DebugContext(ctx, "upstream session DELETE skipped: remote headers not filled in",
				slog.String("instance", row.InstanceID))

			return
		}

		options.Remote = auth
	}

	ctx, cancel := context.WithTimeout(ctx, testtime.Scale(upstreamDeleteTimeout))
	defer cancel()

	session, _, err := ConnectUpstream(ctx, EndpointURL(instance, template), revision, options)
	if err == nil {
		err = session.Close()
	}

	if err != nil {
		opsmetrics.UpstreamSessionEvent(opsmetrics.UpstreamDeleteFailed)
		s.logger().DebugContext(ctx, "upstream session DELETE failed", slog.String("instance", row.InstanceID),
			slog.String("error", redactSessionID(err, row.UpstreamSessionID)))

		return
	}

	opsmetrics.UpstreamSessionEvent(opsmetrics.UpstreamDeleteSent)
}

// redactSessionID returns err's text without the upstream session ID.
func redactSessionID(err error, id string) string {
	text := err.Error()
	if id == "" {
		return text
	}

	return strings.ReplaceAll(text, id, "[session]")
}

func (s *UpstreamSessionSweeper) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}

	return slog.Default()
}
