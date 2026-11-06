//go:build !e2e

package testtime

import "time"

// Scale returns d unchanged. Builds without the e2e tag never read EnvVar.
func Scale(d time.Duration) time.Duration {
	return d
}
