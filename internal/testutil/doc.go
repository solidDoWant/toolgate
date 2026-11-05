// Package testutil starts the dependencies that integration and e2e tests
// need: PostgreSQL that requires TLS client certificates, an S3-compatible
// server and a fake OIDC provider.
//
// The containers run on Docker through testcontainers-go. Each helper takes a
// testing.TB, skips the test with a clear message when Docker is not
// available, and registers a cleanup that removes what it started. The
// testcontainers reaper (Ryuk) also removes the containers when the test
// binary is killed before its cleanups run.
package testutil
