package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// maxWorkspaceBody bounds the gateway's answers to the workspace commands.
const maxWorkspaceBody = 4 << 20

// accessRequestPollInterval is how often the CLI asks whether an access
// request has been decided.
const accessRequestPollInterval = 2 * time.Second

const workspaceUsage = `Usage: toolgate workspace <command>

Commands:
  list                                   Show your defaults and workspace lists
  allow [--workspace ID] SERVER...       Add servers to a workspace's list
  deny [--workspace ID | --session ID] SERVER...
                                         Remove servers from a list, or from one session
  clear [--workspace ID]                 Delete a workspace's list
  defaults [--verified none|all] [--unverified none|all]
                                         Show or set the defaults
`

const messageNoWorkspace = "This machine has no verified workspace. Pass --workspace."

// workspaceEnv is what the workspace commands need besides the gateway.
type workspaceEnv struct {
	stdout, stderr io.Writer

	// wait blocks for d or until ctx ends.
	wait func(ctx context.Context, d time.Duration) error

	// now returns the current time.
	now func() time.Time
}

func realWorkspaceEnv(stdout, stderr io.Writer) workspaceEnv {
	return workspaceEnv{
		stdout: stdout,
		stderr: stderr,
		now:    time.Now,
		wait: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()

			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// workspace runs `toolgate workspace`. It returns the exit code.
func workspace(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, workspaceUsage)

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	env := realWorkspaceEnv(stdout, stderr)

	return runWorkspace(ctx, env, args, func(call func(gatewayAccess) error) error {
		return withAccess(ctx, call)
	})
}

// runWorkspace parses the arguments and runs the command. withAccess supplies
// the credentials.
func runWorkspace(ctx context.Context, env workspaceEnv, args []string, withAccess func(func(gatewayAccess) error) error) int {
	command, rest := args[0], args[1:]

	switch command {
	case "list", "allow", "deny", "clear", "defaults":
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(env.stdout, workspaceUsage)

		return 0
	default:
		_, _ = fmt.Fprintf(env.stderr, "toolgate workspace: unknown command %q\n\n%s", command, workspaceUsage)

		return 2
	}

	options, code, ok := parseWorkspaceArgs(command, rest, env.stderr)
	if !ok {
		return code
	}

	err := withAccess(func(access gatewayAccess) error { return runWorkspaceCommand(ctx, env, access, command, options) })
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "toolgate workspace %s: %s\n", command, errorText(err))

		return 1
	}

	return 0
}

// errorText is the gateway's own message for a refusal, and the error's text
// otherwise.
func errorText(err error) string {
	var refusal *gatewayError
	if errors.As(err, &refusal) && refusal.Message != "" {
		return refusal.Message
	}

	return err.Error()
}

type workspaceOptions struct {
	workspace, session string
	verified           string
	unverified         string
	servers            []string
	workspaceSet       bool
	verifiedSet, uvSet bool
}

func parseWorkspaceArgs(command string, args []string, stderr io.Writer) (workspaceOptions, int, bool) {
	var options workspaceOptions

	flags := flag.NewFlagSet("toolgate workspace "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)

	switch command {
	case "allow", "clear":
		flags.StringVar(&options.workspace, "workspace", "", "workspace `ID` (default: this machine's verified workspace)")
	case "deny":
		flags.StringVar(&options.workspace, "workspace", "", "workspace `ID` (default: this machine's verified workspace)")
		flags.StringVar(&options.session, "session", "", "remove servers added to this session `ID` only")
	case "defaults":
		flags.StringVar(&options.verified, "verified", "", "default for verified workspaces: none or all")
		flags.StringVar(&options.unverified, "unverified", "", "default for unverified sessions: none or all")
	}

	// Flags may come before or after the server names.
	var positional []string

	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return options, 0, false
			}

			return options, 2, false
		}

		if flags.NArg() == 0 {
			break
		}

		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}

	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "workspace":
			options.workspaceSet = true
		case "verified":
			options.verifiedSet = true
		case "unverified":
			options.uvSet = true
		}
	})

	fail := func(format string, a ...any) (workspaceOptions, int, bool) {
		_, _ = fmt.Fprintf(stderr, "toolgate workspace %s: %s\n", command, fmt.Sprintf(format, a...))

		return options, 2, false
	}

	switch command {
	case "allow", "deny":
		if len(positional) == 0 {
			return fail("name at least one server")
		}

		options.servers = positional
	default:
		if len(positional) > 0 {
			return fail("unexpected argument %q", positional[0])
		}
	}

	if options.workspaceSet && options.workspace == "" {
		return fail("--workspace needs a workspace ID")
	}

	if options.session != "" && options.workspaceSet {
		return fail("--session can't be combined with --workspace")
	}

	if command == "defaults" {
		for _, value := range []struct {
			name, value string
			set         bool
		}{{"verified", options.verified, options.verifiedSet}, {"unverified", options.unverified, options.uvSet}} {
			if value.set && value.value != "none" && value.value != "all" {
				return fail("--%s must be none or all", value.name)
			}
		}
	}

	return options, 0, true
}

func runWorkspaceCommand(ctx context.Context, env workspaceEnv, access gatewayAccess, command string, options workspaceOptions) error {
	switch command {
	case "list":
		return printWorkspaces(ctx, env, access)
	case "defaults":
		if !options.verifiedSet && !options.uvSet {
			return printWorkspaces(ctx, env, access)
		}

		return postChange(ctx, env, access, workspacesapi.Change{
			Action: "defaults", Verified: options.verified, Unverified: options.unverified,
		})
	}

	change := workspacesapi.Change{Action: command, Servers: options.servers}

	switch {
	case options.session != "":
		change.Session = options.session
	case options.workspace != "":
		change.Workspace = options.workspace
	default:
		current, err := currentWorkspace(ctx, access)
		if err != nil {
			return err
		}

		change.Workspace = current
	}

	return postChange(ctx, env, access, change)
}

func getWorkspaces(ctx context.Context, access gatewayAccess) (workspacesapi.List, error) {
	response, err := access.do(ctx, http.MethodGet, workspacesapi.WorkspacesPath, nil)
	if err != nil {
		return workspacesapi.List{}, err
	}

	defer func() { _ = response.Body.Close() }()

	var list workspacesapi.List
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&list); err != nil {
		return workspacesapi.List{}, fmt.Errorf("read the gateway's answer: %w", err)
	}

	return list, nil
}

func currentWorkspace(ctx context.Context, access gatewayAccess) (string, error) {
	list, err := getWorkspaces(ctx, access)
	if err != nil {
		return "", err
	}

	if list.Current == nil || *list.Current == "" {
		return "", &gatewayError{Message: messageNoWorkspace}
	}

	return *list.Current, nil
}

func printWorkspaces(ctx context.Context, env workspaceEnv, access gatewayAccess) error {
	list, err := getWorkspaces(ctx, access)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(env.stdout, "Default for verified workspaces: %s\n", list.Defaults.Verified)
	_, _ = fmt.Fprintf(env.stdout, "Default for unverified sessions: %s\n", list.Defaults.Unverified)

	if len(list.Workspaces) == 0 {
		_, _ = fmt.Fprintln(env.stdout, "\nYou have no workspace lists.")

		return nil
	}

	_, _ = fmt.Fprintln(env.stdout)

	table := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "WORKSPACE\tSERVERS\tUPDATED")

	for _, entry := range list.Workspaces {
		servers := "-"
		if len(entry.Servers) > 0 {
			servers = strings.Join(entry.Servers, ",")
		}

		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", entry.Workspace, servers, entry.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if err := table.Flush(); err != nil {
		return errors.Join(errors.New("write the list"), err)
	}

	return nil
}

// postChange sends a change. A 202 makes it wait for the approval.
func postChange(ctx context.Context, env workspaceEnv, access gatewayAccess, change workspacesapi.Change) error {
	body, err := json.Marshal(change)
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodPost, workspacesapi.ChangesPath, bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusAccepted {
		_, _ = fmt.Fprintln(env.stdout, "Done.")

		return nil
	}

	var requested workspacesapi.Requested
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&requested); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	_, _ = fmt.Fprintf(env.stdout, "Approve this change at %s. Waiting… (Ctrl-C stops waiting; the request stays open until it expires.)\n", requested.URL)

	return awaitDecision(ctx, env, access, requested.Request)
}

// awaitDecision polls the access request until it is decided or expired. It
// returns nil only when the request was approved.
func awaitDecision(ctx context.Context, env workspaceEnv, access gatewayAccess, id string) error {
	for {
		status, expiresAt, err := readAccessRequest(ctx, access, id)
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("stopped waiting; the request stays open until it expires")
			}

			return err
		}

		switch status {
		case "approved":
			_, _ = fmt.Fprintln(env.stdout, "Approved. The change is applied.")

			return nil
		case "denied":
			return errors.New("the change was denied")
		case "expired":
			return errors.New("the request expired before anyone approved it")
		}

		if !expiresAt.IsZero() && !env.now().Before(expiresAt) {
			return errors.New("the request expired before anyone approved it")
		}

		if err := env.wait(ctx, accessRequestPollInterval); err != nil {
			return errors.New("stopped waiting; the request stays open until it expires")
		}
	}
}

func readAccessRequest(ctx context.Context, access gatewayAccess, id string) (string, time.Time, error) {
	response, err := access.do(ctx, http.MethodGet, workspacesapi.AccessRequestsPath+id, nil)
	if err != nil {
		return "", time.Time{}, err
	}

	defer func() { _ = response.Body.Close() }()

	var request workspacesapi.AccessRequest
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&request); err != nil {
		return "", time.Time{}, fmt.Errorf("read the gateway's answer: %w", err)
	}

	return request.Status, request.ExpiresAt, nil
}
