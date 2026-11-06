//go:build !e2e

package testtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/testtime"
)

// A binary built without the e2e tag, such as the release image, ignores
// TOOLGATE_TEST_TIME_SCALE.
func TestScaleIgnoresVariableWithoutE2ETag(t *testing.T) {
	for _, value := range []string{"0.1", "0.5", "2", "10"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(testtime.EnvVar, value)

			assert.Equal(t, time.Minute, testtime.Scale(time.Minute))
			assert.Equal(t, 30*time.Second, testtime.Scale(30*time.Second))
		})
	}
}
