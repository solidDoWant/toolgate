//go:build integration

package testutil_test

import (
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

// TestHelpersRemoveContainersOnFailure runs a child test that starts a
// helper and then fails, and checks the helper's container is gone after the
// child exits.
func TestHelpersRemoveContainersOnFailure(t *testing.T) {
	t.Parallel()
	testutil.RequireDocker(t)

	docker, err := client.New(client.FromEnv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = docker.Close() })

	containerID := regexp.MustCompile(`container-id=([0-9a-f]+)`)

	for _, helper := range []string{"postgres", "s3"} {
		t.Run(helper, func(t *testing.T) {
			t.Parallel()

			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStartHelperWithoutDocker$", "-test.v")

			command.Env = append(os.Environ(), skipChildEnv+"="+helper+"-fail")
			output, err := command.CombinedOutput()
			require.Error(t, err, "the child test must fail: %s", output)

			match := containerID.FindSubmatch(output)
			require.NotNil(t, match, "the child test logs its container ID: %s", output)

			_, err = docker.ContainerInspect(t.Context(), string(match[1]), client.ContainerInspectOptions{})
			assert.Error(t, err, "container %s still exists after the failed test", match[1])
		})
	}
}
