package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/audittail"
)

const auditUsage = `Usage: toolgate audit <command>

Commands:
  tail      Print audit events as they happen (tail [--after KEY] [--json])
`

// Bounds of `toolgate audit tail`.
const (
	// maxTailLine bounds one line of the stream.
	maxTailLine = 1 << 20
	// tailMinBackoff and tailMaxBackoff bound the wait before reconnecting
	// after an error.
	tailMinBackoff = time.Second
	tailMaxBackoff = 30 * time.Second
)

// auditCommand runs an audit command and returns the exit code.
func auditCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, auditUsage)

		return 2
	}

	switch args[0] {
	case "tail":
		return auditTail(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, auditUsage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate audit: unknown command %q\n\n%s", args[0], auditUsage)

		return 2
	}
}

// auditTail runs `toolgate audit tail` until Ctrl-C.
func auditTail(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("audit tail", flag.ContinueOnError)
	flags.SetOutput(stderr)
	after := flags.String("after", "", "start after this event key")
	raw := flags.Bool("json", false, "print each event as the gateway's JSON line")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate audit tail: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	if *after != "" {
		if _, _, ok := audittail.ParseKey(*after); !ok {
			_, _ = fmt.Fprintln(stderr, "toolgate audit tail: --after must be an event key, as printed with --json")

			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tail := tailer{after: *after, raw: *raw, stdout: stdout, stderr: stderr, minBackoff: tailMinBackoff, maxBackoff: tailMaxBackoff}
	if err := tail.run(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate audit tail: %v\n", err)

		return 1
	}

	return 0
}

// errTailRefused is an answer that retrying can't change.
var errTailRefused = errors.New("refused")

// tailer follows the audit tail across reconnects.
type tailer struct {
	// after is the key of the last event printed.
	after      string
	raw        bool
	stdout     io.Writer
	stderr     io.Writer
	minBackoff time.Duration
	maxBackoff time.Duration
}

// run streams until ctx ends. After a clean end of a stream it reconnects
// at once with a fresh token and the last key; after an error it waits,
// doubling the wait from minBackoff up to maxBackoff.
func (t *tailer) run(ctx context.Context) error {
	backoff := t.minBackoff

	for {
		err := t.once(ctx)

		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, errNotSignedIn), errors.Is(err, errTailRefused):
			return err
		case err == nil:
			backoff = t.minBackoff

			continue
		}

		_, _ = fmt.Fprintf(t.stderr, "toolgate audit tail: %v; reconnecting in %s\n", err, backoff)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		backoff = min(2*backoff, t.maxBackoff)
	}
}

// once opens one stream and prints its events until it ends. It returns nil
// when the gateway ended the stream cleanly.
func (t *tailer) once(ctx context.Context) error {
	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	target := access.gateway + audittail.Path
	if t.after != "" {
		target += "?after=" + url.QueryEscape(t.after)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}

	request.Header.Set("Authorization", "Bearer "+access.accessToken)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("call the gateway: %w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		err := fmt.Errorf("the gateway answered %d: %s", response.StatusCode, strings.TrimSpace(string(message)))

		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusForbidden {
			return errors.Join(errTailRefused, err)
		}

		return err
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxTailLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var event audittail.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("read the gateway's answer: %w", err)
		}

		if t.raw {
			_, _ = fmt.Fprintf(t.stdout, "%s\n", line)
		} else {
			_, _ = fmt.Fprintln(t.stdout, formatEvent(event))
		}

		t.after = event.Key
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	return nil
}

// formatEvent returns one line: time, type, principal, workspace, session,
// server, tool, and the decision or result. Empty fields print as a dash.
func formatEvent(event audittail.Event) string {
	var details map[string]any

	_ = json.Unmarshal(event.Details, &details)

	outcome := ""

	for _, key := range []string{"decision", "outcome"} {
		if value, ok := details[key].(string); ok && value != "" {
			outcome = value

			break
		}
	}

	fields := []string{
		event.OccurredAt.UTC().Format(time.RFC3339), event.Type, event.Principal, event.Workspace,
		event.Session, event.Server, event.Tool, outcome,
	}

	for index, field := range fields {
		if field == "" {
			fields[index] = "-"
		}
	}

	return strings.Join(fields, " ")
}
