//go:build e2e

package testtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/testtime"
)

// A binary built with the e2e tag scales durations by
// TOOLGATE_TEST_TIME_SCALE, and leaves them unchanged when the value is
// missing or not a positive number.
func TestScaleHonoursVariableWithE2ETag(t *testing.T) {
	tests := []struct {
		name  string
		value string
		in    time.Duration
		want  time.Duration
	}{
		{name: "tenth", value: "0.1", in: time.Minute, want: 6 * time.Second},
		{name: "half", value: "0.5", in: 30 * time.Second, want: 15 * time.Second},
		{name: "one", value: "1", in: time.Minute, want: time.Minute},
		{name: "unset", value: "", in: time.Minute, want: time.Minute},
		{name: "zero", value: "0", in: time.Minute, want: time.Minute},
		{name: "negative", value: "-0.1", in: time.Minute, want: time.Minute},
		{name: "not a number", value: "fast", in: time.Minute, want: time.Minute},
		{name: "infinite", value: "Inf", in: time.Minute, want: time.Minute},
		{name: "NaN", value: "NaN", in: time.Minute, want: time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(testtime.EnvVar, test.value)

			assert.Equal(t, test.want, testtime.Scale(test.in))
		})
	}
}
