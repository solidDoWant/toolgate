package main

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// testEnv is a process environment for one test. A test sets what it needs
// before it runs a command, so tests run in parallel.
type testEnv map[string]string

func (e testEnv) get(name string) string { return e[name] }

func (e testEnv) set(name, value string) { e[name] = value }

// environment is what the test helpers need of an environment: the lookup the
// commands get and a way to set a variable.
type environment interface {
	get(name string) string
	set(name, value string)
}

// processEnv is the real process environment, for the tests whose subject
// also reads it (the gateway's configuration). Such a test cannot run in
// parallel.
type processEnv struct{ t *testing.T }

func (e processEnv) get(name string) string { return os.Getenv(name) }

func (e processEnv) set(name, value string) { e.t.Setenv(name, value) }

// TestMain replaces the OS keychain with an in-memory one. The keychain is
// process-wide, so a test that changes it or depends on its contents does not
// call t.Parallel.
func TestMain(m *testing.M) {
	keyring.MockInit()

	os.Exit(m.Run())
}
