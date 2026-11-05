package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Instance statuses an enable waits for.
const (
	statusReady  = "ready"
	statusFailed = "failed"
)

// waitForInstance runs Enable step 5. It re-reads the instance and the
// session's session_servers row on every notification about either and on
// every progress tick, and returns once the instance is ready or no longer
// starting. A progress notification goes out on every status change and at
// least every progressInterval.
func (m *metaTools) waitForInstance(ctx context.Context, request *mcp.CallToolRequest, current requestContext,
	template *v1alpha1.MCPServerTemplate, instance db.Instance,
) (db.Instance, error) {
	events, unsubscribe := m.subscribe()
	defer unsubscribe()

	ctx, cancel := context.WithTimeout(ctx, startTimeout(template))
	defer cancel()

	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()

	sessionID := strconv.FormatInt(current.session.ID, 10)
	progress := progressSender{request: request}
	lastStatus := ""

	for {
		latest, done, err := m.readWait(ctx, current, template, instance.ID)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return db.Instance{}, timedOut(template)
			}

			return db.Instance{}, err
		}

		if done {
			return latest, nil
		}

		if latest.Status != lastStatus {
			lastStatus = latest.Status
			progress.send(ctx, template.Name+" is "+latest.Status)
		}

		ticked, err := awaitWake(ctx, events, ticker.C, instance.ID, sessionID)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return db.Instance{}, timedOut(template)
			}

			return db.Instance{}, err
		}

		if ticked {
			progress.send(ctx, template.Name+" is "+latest.Status)
		}
	}
}

// awaitWake blocks until a notification about the instance or the session
// arrives, notifications were missed, or the progress tick fires. ticked
// is true for the tick.
func awaitWake(ctx context.Context, events <-chan store.Event, tick <-chan time.Time, instanceID, sessionID string) (ticked bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick:
			return true, nil
		case event := <-events:
			if event.Missed || (event.Kind == store.KindInstance && event.ID == instanceID) ||
				(event.Kind == store.KindSession && event.ID == sessionID) {
				return false, nil
			}
		}
	}
}

// readWait re-reads the rows a waiting enable depends on. done is true when
// waiting is over: the instance is ready, or it can no longer become ready.
func (m *metaTools) readWait(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, id string) (db.Instance, bool, error) {
	queries := db.New(m.database)

	enabled, err := m.isEnabled(ctx, queries, current.session.ID, template)
	if err != nil {
		return db.Instance{}, false, err
	}

	if !enabled {
		return db.Instance{}, false, fmt.Errorf("`%s` was disabled while starting.", template.Name) //nolint:staticcheck // A sentence for the model.
	}

	instance, err := queries.GetInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, true, nil
	}

	if err != nil {
		return db.Instance{}, false, fmt.Errorf("read instance: %w", err)
	}

	switch instance.Status {
	case "pending", "starting":
		return instance, instance.Desired != "running", nil
	default:
		return instance, true, nil
	}
}

func startTimeout(template *v1alpha1.MCPServerTemplate) time.Duration {
	if template.Spec.StartTimeout != nil && template.Spec.StartTimeout.Duration > 0 {
		return template.Spec.StartTimeout.Duration
	}

	return defaultStartTimeout
}

func timedOut(template *v1alpha1.MCPServerTemplate) error {
	return fmt.Errorf("`%s` didn't become ready within %s. Check it with `%s`.", template.Name, startTimeout(template), ToolServerStatus) //nolint:staticcheck // A sentence for the model.
}

// progressSender sends progress notifications for one tool call on its own
// response stream. A call without a progress token gets none.
type progressSender struct {
	request *mcp.CallToolRequest
	count   float64
}

func (p *progressSender) send(ctx context.Context, message string) {
	token := p.request.Params.GetProgressToken()
	if token == nil || p.request.Session == nil {
		return
	}

	p.count++

	_ = p.request.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      p.count,
		Message:       message,
	})
}
