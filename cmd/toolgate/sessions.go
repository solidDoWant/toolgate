package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/solidDoWant/toolgate/internal/sessionsapi"
)

// maxSessionsBody bounds the sessions response: 200 sessions of up to 32
// servers each fit.
const maxSessionsBody = 4 << 20

// sessions runs `toolgate sessions`: it prints the signed-in user's open
// sessions and their enabled servers. It returns the exit code.
func sessions(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate sessions: unexpected argument %q\n", args[0])

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := listSessions(ctx, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate sessions: %v\n", err)

		return 1
	}

	return 0
}

func listSessions(ctx context.Context, stdout io.Writer) error {
	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodGet, sessionsapi.Path, nil)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	var list sessionsapi.List
	if err := json.NewDecoder(io.LimitReader(response.Body, maxSessionsBody)).Decode(&list); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	if len(list.Sessions) == 0 {
		_, _ = fmt.Fprintln(stdout, "You have no open sessions.")

		return nil
	}

	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "SESSION\tWORKSPACE\tLAST SEEN\tSERVERS")

	for _, session := range list.Sessions {
		names := make([]string, 0, len(session.Servers))
		for _, server := range session.Servers {
			names = append(names, server.Name)
		}

		servers := "-"
		if len(names) > 0 {
			servers = strings.Join(names, ",")
		}

		workspace := session.Workspace
		if workspace == "" {
			workspace = "-"
		}

		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", session.Session, workspace, session.LastSeenAt.UTC().Format(time.RFC3339), servers)
	}

	if err := table.Flush(); err != nil {
		return errors.Join(errors.New("write the list"), err)
	}

	if list.Truncated {
		_, _ = fmt.Fprintf(stdout, "\nShowing the newest %d sessions. You have more.\n", sessionsapi.MaxSessions)
	}

	return nil
}
