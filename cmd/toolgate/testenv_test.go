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

// runAsBinaryEnv makes the test binary run main instead of the tests.
const runAsBinaryEnv = "TOOLGATE_TEST_RUN_MAIN"

// TestMain replaces the OS keychain with an in-memory one. The keychain is
// process-wide, so a test that changes it or depends on its contents does not
// call t.Parallel.
func TestMain(m *testing.M) {
	// A test that needs a real process (to send it SIGTERM) runs this test
	// binary as the toolgate command.
	if os.Getenv(runAsBinaryEnv) != "" {
		main()

		return
	}

	keyring.MockInit()

	os.Exit(m.Run())
}
