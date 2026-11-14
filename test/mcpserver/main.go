// Command mcpserver is the MCP server the e2e tests run as a catalog
// template's workload. Its tools report what the gateway rendered into the
// pod: the env tool returns the environment, the secrets tool returns the
// files of a mounted Secret, and every other configured tool name echoes its
// arguments. It serves streamable HTTP at /mcp.
//
// GET /healthz answers with the health status, 200 at start. PUT
// /healthz?status=503 sets it, so a test can make a health check fail and
// pass again.
//
// GET /calls answers with the number of calls to each tool, as a JSON object
// from tool name to count.
//
// Modes. The default mode serves the tools named by -tools. With
// -mode 2026-only the server is stateless and speaks only protocol revision
// 2026-07-28. With -mode 2025-only the server is stateful, so it speaks
// 2025-11-25 and older revisions only. With -mode both the server is
// stateful on 2025-11-25 and stateless on 2026-07-28, so it speaks both.
// With -tools-file the server serves the tools declared in that
// file, which is a JSON array of {name, description, inputSchema} (or an
// object with that array in "tools"); every such tool echoes its arguments.
// In each of those cases, PUT /tools replaces the tool list and
// notifies the connected sessions, with a body in the file's format or
// ?names=a,b for echo tools with no schema. A server in a mode other than
// default and without -tools-file serves the names of -tools as echo tools.
//
// With -bearer-tokens a request to /mcp needs "Authorization: Bearer TOKEN"
// for one of the configured tokens, and gets 401 otherwise. The whoami tool
// returns the user of the token, so a test can check which key reached the
// server.
//
// The count tool is marked readOnlyHint and returns how often it was
// called, so a test sees whether a call reached the server. -count-ttl sets
// its cache lifetime suggestion.
//
// Configuration, by flag or environment variable:
//
//	-listen      MCPSERVER_LISTEN       listen address (default :8080)
//	-tools       MCPSERVER_TOOLS        comma-separated tool names (default env,secrets)
//	-secrets-dir MCPSERVER_SECRETS_DIR  directory the secrets tool reads (default /secrets)
//	-mode        MCPSERVER_MODE        default, 2026-only, 2025-only or both
//	-tools-file  MCPSERVER_TOOLS_FILE  file with the tool declarations
//	-bearer-tokens MCPSERVER_BEARER_TOKENS  token=user pairs; /mcp answers 401 to any other bearer token
//	-health      MCPSERVER_HEALTH       status /healthz answers at start (default 200)
//	-count-ttl   MCPSERVER_COUNT_TTL    the count tool's `_meta["toolgate/cache"].ttl` suggestion
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	mode := flags.String("mode", envOr("MCPSERVER_MODE", modeDefault), "default, 2026-only, 2025-only or both")
	toolsFile := flags.String("tools-file", envOr("MCPSERVER_TOOLS_FILE", ""), "file with the tool declarations")
	bearerTokens := flags.String("bearer-tokens", envOr("MCPSERVER_BEARER_TOKENS", ""), "token=user pairs; /mcp refuses other tokens")
	health := flags.String("health", envOr("MCPSERVER_HEALTH", "200"), "status /healthz answers at start")
	countTTL := flags.String("count-ttl", envOr("MCPSERVER_COUNT_TTL", ""), "the count tool's toolgate/cache ttl suggestion")

	if err := flags.Parse(args); err != nil {
		return err
	}

	mux, err := newHandler(handlerConfig{
		Mode: *mode, Tools: *tools, ToolsFile: *toolsFile, SecretsDir: *secretsDir, Health: *health, BearerTokens: *bearerTokens,
		CountTTL: *countTTL,
	})
	if err != nil {
		return err
	}

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

func parseStatus(text string) (int, error) {
	status, err := strconv.Atoi(text)
	if err != nil || status < 100 || status > 599 {
		return 0, fmt.Errorf("health status %q is not an HTTP status", text)
	}

	return status, nil
}

// Server modes.
const (
	modeDefault  = "default"
	mode2026Only = "2026-only"
	// mode2025Only is a stateful server, which speaks 2025-11-25 and older
	// revisions, with a tool list that PUT /tools replaces.
	mode2025Only = "2025-only"
	// modeBoth is a stateless server that speaks 2025-11-25 and 2026-07-28.
	modeBoth = "both"
)

// toolSpecsFor returns the tools of the file, or echo tools named names.
func toolSpecsFor(file string, names []string) ([]toolSpec, error) {
	if file != "" {
		return readToolSpecs(file)
	}

	specs := echoSpecs(names)

	return specs, validateSpecs(specs)
}

func echoSpecs(names []string) []toolSpec {
	specs := make([]toolSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, toolSpec{Name: name, Description: "Echoes its arguments."})
	}

	return specs
}

// specsFromRequest reads the new tool list of a PUT /tools request.
func specsFromRequest(writer http.ResponseWriter, request *http.Request) ([]toolSpec, error) {
	if names, ok := request.URL.Query()["names"]; ok {
		if len(names) != 1 {
			return nil, errors.New("names is given once")
		}

		specs := echoSpecs(parseTools(names[0]))

		return specs, validateSpecs(specs)
	}

	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxToolsFileSize))
	if err != nil {
		return nil, err
	}

	return parseToolSpecs(body)
}

// handlerConfig configures the HTTP handler of the test MCP server.
type handlerConfig struct {
	Mode       string
	Tools      string
	ToolsFile  string
	SecretsDir string
	Health     string
	// BearerTokens is a comma-separated list of token=user pairs. When it
	// is set, /mcp answers 401 unless the request carries one of the tokens
	// as a bearer token.
	BearerTokens string
	// CountTTL is the count tool's cache lifetime suggestion.
	CountTTL string
}

// newHandler returns the server's HTTP handler: /mcp, /calls, /tools,
// /healthz.
func newHandler(config handlerConfig) (*http.ServeMux, error) {
	if config.Mode == "" {
		config.Mode = modeDefault
	}

	if !slices.Contains([]string{modeDefault, mode2026Only, mode2025Only, modeBoth}, config.Mode) {
		return nil, fmt.Errorf("mode %q is not %s, %s, %s or %s", config.Mode, modeDefault, mode2026Only, mode2025Only, modeBoth)
	}

	calls := &callCounter{}

	users, err := parseBearerTokens(config.BearerTokens)
	if err != nil {
		return nil, err
	}

	var (
		server *mcp.Server
		set    *toolSet
	)

	var versions []string

	handlerOptions := &mcp.StreamableHTTPOptions{}

	switch config.Mode {
	case mode2026Only:
		versions = []string{onlyProtocolVersion}
		handlerOptions.Stateless = true
	}

	if config.ToolsFile != "" || config.Mode != modeDefault {
		specs, specsErr := toolSpecsFor(config.ToolsFile, parseTools(config.Tools))
		if specsErr != nil {
			return nil, specsErr
		}

		server, set, err = newToolListServer(specs, calls, versions)
	} else {
		server, err = newServer(options{Tools: parseTools(config.Tools), SecretsDir: config.SecretsDir, Environ: os.Environ, Calls: calls, Users: users, CountTTL: config.CountTTL})
	}

	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	handler := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, handlerOptions))
	if config.Mode == modeBoth {
		handler = bothRevisions(server, handler)
	}

	if len(users) > 0 {
		handler = requireBearer(users, handler)
	}

	mux.Handle("/mcp", handler)
	mux.HandleFunc("GET /calls", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(calls.snapshot())
	})

	if set != nil {
		mux.HandleFunc("PUT /tools", func(writer http.ResponseWriter, request *http.Request) {
			specs, err := specsFromRequest(writer, request)
			if err == nil {
				err = set.set(specs)
			}

			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)

				return
			}

			writer.WriteHeader(http.StatusNoContent)
		})
	}

	healthStatus, err := parseStatus(config.Health)
	if err != nil {
		return nil, err
	}

	var status atomic.Int64

	status.Store(int64(healthStatus))

	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(int(status.Load()))
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("PUT /healthz", func(writer http.ResponseWriter, request *http.Request) {
		next, err := parseStatus(request.URL.Query().Get("status"))
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		status.Store(int64(next))
		writer.WriteHeader(http.StatusNoContent)
	})

	return mux, nil
}

// bothRevisions serves a request on 2026-07-28 or later statelessly, and
// every other request with stateful, so one server speaks 2025-11-25 with
// sessions and 2026-07-28 without them. The go-sdk's stateful handler
// doesn't serve 2026-07-28, and its stateless one has no stream for
// `list_changed` on 2025-11-25.
func bothRevisions(server *mcp.Server, stateful http.Handler) http.Handler {
	stateless := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true})

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Mcp-Protocol-Version") >= onlyProtocolVersion {
			stateless.ServeHTTP(writer, request)

			return
		}

		stateful.ServeHTTP(writer, request)
	})
}

const bearerPrefix = "Bearer "

// parseBearerTokens parses comma-separated token=user pairs.
func parseBearerTokens(list string) (map[string]string, error) {
	users := map[string]string{}

	for pair := range strings.SplitSeq(list, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}

		token, user, found := strings.Cut(pair, "=")
		if !found || token == "" || user == "" {
			return nil, fmt.Errorf("bearer token entry %q is not token=user", pair)
		}

		users[token] = user
	}

	return users, nil
}

// requireBearer refuses a request without one of the users' tokens.
func requireBearer(users map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, found := strings.CutPrefix(request.Header.Get("Authorization"), bearerPrefix)
		if _, known := users[token]; !found || !known {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(writer, request)
	})
}
