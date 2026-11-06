package testutil_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

const skipChildEnv = "TOOLGATE_TESTUTIL_NO_DOCKER_CHILD"

// TestHelpersSkipWithoutDocker runs the container helpers in a child test
// process whose Docker host does not exist, and checks they skip with a clear
// message instead of failing.
func TestHelpersSkipWithoutDocker(t *testing.T) {
	t.Parallel()

	helpers := []string{"postgres", "s3"}
	for _, helper := range helpers {
		t.Run(helper, func(t *testing.T) {
			t.Parallel()

			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStartHelperWithoutDocker$", "-test.v")

			command.Env = append(os.Environ(),
				skipChildEnv+"="+helper,
				"DOCKER_HOST=unix:///nonexistent/docker.sock",
			)
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)

			assert.Contains(t, string(output), "--- SKIP: TestStartHelperWithoutDocker")
			assert.Contains(t, string(output), "Docker is not available")
		})
	}
}

// TestStartHelperWithoutDocker is the child test process of
// TestHelpersSkipWithoutDocker and TestHelpersRemoveContainersOnFailure. It
// starts the helper named in its environment and then fails.
func TestStartHelperWithoutDocker(t *testing.T) {
	switch os.Getenv(skipChildEnv) {
	case "":
		t.Skip("runs only as a child test process")
	case "postgres":
		testutil.StartPostgres(t)
	case "s3":
		testutil.StartS3(t)
	case "postgres-fail":
		t.Logf("container-id=%s", testutil.StartPostgresDedicated(t).ContainerID)
	case "s3-fail":
		t.Logf("container-id=%s", testutil.StartS3(t).ContainerID)
	}

	t.Fatal("the test failed after starting its dependencies")
}
