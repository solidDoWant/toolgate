package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// RequireDocker skips tb when no healthy Docker daemon is reachable.
//
// When DOCKER_HOST is set, only that daemon counts: testcontainers would
// otherwise fall back to the default socket, and tests would run against a
// daemon the caller did not choose.
func RequireDocker(tb testing.TB) {
	tb.Helper()

	if err := dockerHealthy(tb.Context()); err != nil {
		tb.Skipf("integration dependencies need Docker, and Docker is not available: %v", err)
	}
}

func dockerHealthy(ctx context.Context) (err error) {
	// The Docker clients panic instead of returning an error for some broken
	// configurations.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("docker client panicked: %v", recovered)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return pingDockerHost(ctx, host)
	}

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()

	return provider.Health(ctx)
}

func pingDockerHost(ctx context.Context, host string) error {
	docker, err := client.New(client.WithHost(host))
	if err != nil {
		return fmt.Errorf("DOCKER_HOST %q: %w", host, err)
	}
	defer func() { _ = docker.Close() }()

	if _, err := docker.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("DOCKER_HOST %q: %w", host, err)
	}

	return nil
}
