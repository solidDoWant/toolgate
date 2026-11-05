package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const adminUsage = `Usage: toolgate admin <command>

Commands:
  rewrap    Re-encrypt every user secret under the keyset's primary key
            (rewrap --config FILE)
`

// admin runs an admin command and returns the exit code.
func admin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, adminUsage)

		return 2
	}

	switch args[0] {
	case "rewrap":
		return rewrap(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, adminUsage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate admin: unknown command %q\n\n%s", args[0], adminUsage)

		return 2
	}
}

// rewrap re-encrypts every user secret not under the primary key of the
// gateway's keyset. It reads the gateway's configuration.
func rewrap(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin rewrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", os.Getenv(config.FileEnv),
		"path to the gateway's YAML configuration file (default from "+config.FileEnv+")")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate admin rewrap: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	cfg, err := config.Load(*configPath, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate admin rewrap: invalid configuration:\n%s\n", err)

		return 1
	}

	if cfg.SecretsKeysetFile == "" {
		_, _ = fmt.Fprintln(stderr, "toolgate admin rewrap: secretsKeysetFile is not set in the configuration")

		return 1
	}

	cipher, err := secrets.LoadKeyset(cfg.SecretsKeysetFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate admin rewrap: %s\n", err)

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		// The error can echo the connection string, so it is not printed.
		_, _ = fmt.Fprintln(stderr, "toolgate admin rewrap: cannot create the PostgreSQL connection pool")

		return 1
	}
	defer pool.Close()

	count, err := secrets.NewBroker(db.New(pool), cipher).Rewrap(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate admin rewrap: %s (%d secrets re-encrypted before the error)\n", err, count)

		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Re-encrypted %d secrets under key %d. Keys other than the primary key can now be removed from the keyset.\n",
		count, cipher.PrimaryKeyID())

	return 0
}
