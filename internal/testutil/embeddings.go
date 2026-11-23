package testutil

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// EmbeddingsImage is the embeddings server the integration tests run:
// Hugging Face text-embeddings-inference on CPU, pinned by digest.
const EmbeddingsImage = "ghcr.io/huggingface/text-embeddings-inference:cpu-1.9@sha256:2538ea1c9640d3763b15af668039d24172d063b42337b0c27796fc2be180c78d"

// EmbeddingsModel is the model the embeddings server loads.
const EmbeddingsModel = "BAAI/bge-small-en-v1.5"

const embeddingsPort = "80/tcp"

// Embeddings is a running server of the OpenAI embeddings API.
type Embeddings struct {
	// URL is the base URL; the API is at URL + "/v1/embeddings".
	URL   string
	Model string
}

// StartEmbeddings starts the embeddings server in Docker and removes it when
// tb ends. The model is downloaded once into a cache directory
// (`~/.cache/toolgate-embeddings`, which CI caches between runs). It skips
// tb when Docker is not available.
func StartEmbeddings(tb testing.TB) *Embeddings {
	tb.Helper()

	return startModelServer(tb, EmbeddingsModel)
}

func startModelServer(tb testing.TB, model string) *Embeddings {
	tb.Helper()
	RequireDocker(tb)

	cacheRoot, err := os.UserCacheDir()
	require.NoError(tb, err)

	cache := filepath.Join(cacheRoot, "toolgate-embeddings")
	require.NoError(tb, os.MkdirAll(cache, 0o777))
	// The server runs as another user in the container.
	require.NoError(tb, os.Chmod(cache, 0o777))

	ctx := tb.Context()

	// The server downloads the model on first start and can exit when the
	// download or the machine is under load, so a failed start is retried
	// and its logs are reported.
	var (
		server   testcontainers.Container
		startErr error
	)

	for attempt := 1; attempt <= embeddingsStartAttempts; attempt++ {
		server, startErr = testcontainers.Run(ctx, EmbeddingsImage,
			testcontainers.WithCmd("--model-id", model, "--port", "80"),
			testcontainers.WithHostConfigModifier(func(config *container.HostConfig) {
				config.Binds = append(config.Binds, cache+":/data")
			}),
			testcontainers.WithExposedPorts(embeddingsPort),
			testcontainers.WithWaitStrategy(
				wait.ForHTTP("/health").WithPort(embeddingsPort).WithStartupTimeout(2*time.Minute),
			),
		)
		testcontainers.CleanupContainer(tb, server)

		if startErr == nil {
			break
		}

		tb.Logf("start embeddings server, attempt %d of %d: %v%s", attempt, embeddingsStartAttempts, startErr, containerLogs(ctx, server))
	}

	require.NoError(tb, startErr, "start embeddings server")

	endpoint, err := server.PortEndpoint(ctx, embeddingsPort, "http")
	require.NoError(tb, err)

	return &Embeddings{URL: endpoint, Model: model}
}

const embeddingsStartAttempts = 3

// containerLogs returns the tail of a container's logs for an error message.
func containerLogs(ctx context.Context, started testcontainers.Container) string {
	if started == nil {
		return ""
	}

	reader, err := started.Logs(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = reader.Close() }()

	data, _ := io.ReadAll(io.LimitReader(reader, 1<<20))
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}

	return "\nlogs:\n" + string(data)
}
