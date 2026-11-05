// Command mcpserver is the MCP server the e2e tests run as a catalog
// template's workload. Its tools report what the gateway rendered into the
// pod: the env tool returns the environment, the secrets tool returns the
// files of a mounted Secret, and every other configured tool name echoes its
// arguments. It serves streamable HTTP at /mcp.
//
// Configuration, by flag or environment variable:
//
//	-listen      MCPSERVER_LISTEN       listen address (default :8080)
//	-tools       MCPSERVER_TOOLS        comma-separated tool names (default env,secrets)
//	-secrets-dir MCPSERVER_SECRETS_DIR  directory the secrets tool reads (default /secrets)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mcpserver:", err)

		os.Exit(1)
	}
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}

	return fallback
}

func run(args []string) error {
	flags := flag.NewFlagSet("mcpserver", flag.ContinueOnError)
	listen := flags.String("listen", envOr("MCPSERVER_LISTEN", ":8080"), "listen address")
	tools := flags.String("tools", envOr("MCPSERVER_TOOLS", envTool+","+secretsTool), "comma-separated tool names")
	secretsDir := flags.String("secrets-dir", envOr("MCPSERVER_SECRETS_DIR", "/secrets"), "directory the secrets tool reads")

	if err := flags.Parse(args); err != nil {
		return err
	}

	server, err := newServer(options{Tools: parseTools(*tools), SecretsDir: *secretsDir, Environ: os.Environ})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok\n"))
	})

	httpServer := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = httpServer.Shutdown(shutdown)
	}()

	slog.Info("mcpserver listening", "address", *listen)

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}
