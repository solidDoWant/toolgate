//go:build e2e

package testtime

import (
	"math"
	"os"
	"strconv"
	"time"
)

// Scale returns d multiplied by the value of EnvVar. It returns d unchanged
// when the variable is unset or is not a finite positive number.
func Scale(d time.Duration) time.Duration {
	factor, err := strconv.ParseFloat(os.Getenv(EnvVar), 64)
	if err != nil || factor <= 0 || math.IsInf(factor, 0) || math.IsNaN(factor) {
		return d
	}

	return time.Duration(float64(d) * factor)
}
