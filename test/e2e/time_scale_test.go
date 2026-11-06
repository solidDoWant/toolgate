//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/internal/testtime"
)

// scaled returns d multiplied by TOOLGATE_TEST_TIME_SCALE, the same scale
// the e2e gateway image applies to its waits. Tests derive their deadlines
// for scaled product waits from it.
func scaled(d time.Duration) time.Duration {
	return testtime.Scale(d)
}

// The gateway under test and the test process read the same time scale, so
// deadlines the tests derive with scaled match the gateway's waits.
func TestTimeScaleMatchesGateway(t *testing.T) {
	value := os.Getenv(testtime.EnvVar)
	if value == "" {
		t.Skipf("%s is not set (no cluster made by scripts/e2e/run.sh)", testtime.EnvVar)
	}

	admin := adminClient(t)
	deployment, err := admin.AppsV1().Deployments(releaseNamespace()).Get(t.Context(), gatewayDeployment, metav1.GetOptions{})
	require.NoError(t, err)

	var gatewayValue string

	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == testtime.EnvVar {
				gatewayValue = env.Value
			}
		}
	}

	assert.Equal(t, value, gatewayValue, "the gateway's %s", testtime.EnvVar)
	assert.Less(t, scaled(time.Minute), time.Minute, "the e2e harness shortens product waits")
}
