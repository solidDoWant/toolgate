// Package version reports the version this binary was built as.
package version

// version is set at build time with
// -ldflags "-X github.com/solidDoWant/toolgate/internal/version.version=x.y.z".
var version = ""

// Get returns the build version, or "dev" for a build without one.
func Get() string {
	if version == "" {
		return "dev"
	}

	return version
}
