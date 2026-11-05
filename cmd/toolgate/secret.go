package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/solidDoWant/toolgate/internal/secrets"
)

const secretUsage = `Usage: toolgate secret <command>

Commands:
  set NAME      Store a secret. The value is read from standard input;
                from a terminal it is read without showing it.
  list          List your secrets: names and dates only
  delete NAME   Delete a secret
`

// secretsPath is the gateway's secrets API.
const secretsPath = "/api/v1/secrets"

// secret runs a `toolgate secret` command and returns the exit code.
func secret(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, secretUsage)

		return 2
	}

	command, rest := args[0], args[1:]

	switch command {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, secretUsage)

		return 0
	case "set", "delete":
		if len(rest) != 1 {
			_, _ = fmt.Fprintf(stderr, "toolgate secret %s: give exactly one secret name\n", command)

			return 2
		}

		if !secrets.ValidName(rest[0]) {
			_, _ = fmt.Fprintf(stderr, "toolgate secret %s: the name must match ^[A-Za-z0-9._-]{1,128}$\n", command)

			return 2
		}
	case "list":
		if len(rest) > 0 {
			_, _ = fmt.Fprintf(stderr, "toolgate secret list: unexpected argument %q\n", rest[0])

			return 2
		}
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate secret: unknown command %q\n\n%s", command, secretUsage)

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error

	switch command {
	case "set":
		err = setSecret(ctx, rest[0], stdin, stderr, stdout)
	case "delete":
		err = deleteSecret(ctx, rest[0], stdout)
	default:
		err = listSecrets(ctx, stdout)
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate secret %s: %v\n", command, err)

		return 1
	}

	return 0
}

// readSecretValue reads the value from stdin. From a terminal it reads one
// line without echo, and refuses when echo can't be turned off. Otherwise it
// reads everything, as is, up to the gateway's limit.
func readSecretValue(name string, stdin io.Reader, prompt io.Writer) ([]byte, error) {
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) { //nolint:gosec // A file descriptor fits in an int.
		_, _ = fmt.Fprintf(prompt, "Value for secret %s (input hidden): ", name)

		value, err := term.ReadPassword(int(file.Fd())) //nolint:gosec // A file descriptor fits in an int.

		_, _ = fmt.Fprintln(prompt)

		if err != nil {
			return nil, fmt.Errorf("can't read the value without showing it (%w); pipe it in instead", err)
		}

		return value, nil
	}

	value, err := io.ReadAll(io.LimitReader(stdin, secrets.MaxValueBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the value from standard input: %w", err)
	}

	if len(value) > secrets.MaxValueBytes {
		return nil, fmt.Errorf("the value is longer than %d bytes", secrets.MaxValueBytes)
	}

	return value, nil
}

func setSecret(ctx context.Context, name string, stdin io.Reader, prompt, stdout io.Writer) error {
	value, err := readSecretValue(name, stdin, prompt)
	if err != nil {
		return err
	}

	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodPut, secretsPath+"/"+url.PathEscape(name), bytes.NewReader(value))
	if err != nil {
		return err
	}

	_ = response.Body.Close()

	_, _ = fmt.Fprintf(stdout, "Secret %s is set.\n", name)

	return nil
}

func deleteSecret(ctx context.Context, name string, stdout io.Writer) error {
	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodDelete, secretsPath+"/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}

	_ = response.Body.Close()

	_, _ = fmt.Fprintf(stdout, "Secret %s is deleted.\n", name)

	return nil
}

// maxSecretListBody bounds the list response. 10,000 entries of the longest
// name and two dates fit.
const maxSecretListBody = 4 << 20

func listSecrets(ctx context.Context, stdout io.Writer) error {
	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodGet, secretsPath, nil)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	var list secrets.SecretList
	if err := json.NewDecoder(io.LimitReader(response.Body, maxSecretListBody)).Decode(&list); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	if len(list.Secrets) == 0 {
		_, _ = fmt.Fprintln(stdout, "You have no secrets. Set one with toolgate secret set NAME.")

		return nil
	}

	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tCREATED\tUPDATED")

	for _, info := range list.Secrets {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", info.Name, info.CreatedAt.UTC().Format(time.RFC3339), info.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if err := table.Flush(); err != nil {
		return errors.Join(errors.New("write the list"), err)
	}

	return nil
}
