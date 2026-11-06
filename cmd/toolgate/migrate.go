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
	"github.com/solidDoWant/toolgate/internal/store"
)

// migrate applies the database migrations and creates the audit partitions,
// then returns the exit code. It reads only databaseURL, so a Job needs no
// other configuration.
func migrate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", os.Getenv(config.FileEnv),
		"path to the gateway's YAML configuration file (default from "+config.FileEnv+")")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate migrate: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	databaseURL, err := config.LoadDatabaseURL(*configPath, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate migrate: invalid configuration:\n%s\n", err)

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		// The error can echo the connection string, so it is not printed.
		_, _ = fmt.Fprintln(stderr, "toolgate migrate: cannot create the PostgreSQL connection pool")

		return 1
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate migrate: %s\n", err)

		return 1
	}

	_, _ = fmt.Fprintln(stdout, "The database schema is up to date.")

	return 0
}
