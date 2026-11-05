// Package health serves the liveness and readiness probes.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// checkTimeout bounds each readiness check.
const checkTimeout = 2 * time.Second

// Check reports whether one dependency the gateway needs is available.
type Check struct {
	// Name identifies the dependency in logs and in the /readyz body.
	Name string
	// Run returns an error while the dependency is unavailable.
	Run func(ctx context.Context) error
}

// Register adds `/healthz` and `/readyz` to mux. `/healthz` answers 200 while
// the process serves requests. `/readyz` answers 200 when every check passes
// and 503 naming the failed dependencies otherwise.
func Register(mux *http.ServeMux, logger *slog.Logger, checks ...Check) {
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")

		var failed []string

		for _, check := range checks {
			ctx, cancel := context.WithTimeout(request.Context(), checkTimeout)
			err := check.Run(ctx)

			cancel()

			if err != nil {
				// The error text can name hosts and users, so only the
				// dependency's name goes into the response.
				logger.WarnContext(request.Context(), "readiness check failed", "check", check.Name, "error", err)

				failed = append(failed, check.Name)
			}
		}

		if len(failed) > 0 {
			writer.WriteHeader(http.StatusServiceUnavailable)

			for _, name := range failed {
				_, _ = writer.Write([]byte(name + ": unavailable\n"))
			}

			return
		}

		_, _ = writer.Write([]byte("ok\n"))
	})
}
