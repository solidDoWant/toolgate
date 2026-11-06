package runtime_test

import (
	"os"
	"testing"

	"github.com/solidDoWant/toolgate/internal/testtime"
)

// TestMain clears the test time scale. The unit tests check the runtime's
// real waits and minimums, so they run unscaled in an e2e-tagged build too.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(testtime.EnvVar)

	os.Exit(m.Run())
}
