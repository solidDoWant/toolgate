package headers_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/headers"
)

// sessionDir points the runtime directory at a new directory and returns
// the directory session files go in.
func sessionDir(t *testing.T) string {
	t.Helper()

	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv(headers.SessionEnv, "")

	dir := filepath.Join(runtimeDir, "toolgate")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	return dir
}

func writeSession(t *testing.T, dir string, pid int, session string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)), []byte(session), 0o600))
}

// grandparent returns the parent of this process's parent.
func grandparent(t *testing.T) int {
	t.Helper()

	parent, err := process.NewProcessWithContext(t.Context(), int32(os.Getppid())) //nolint:gosec // PIDs fit in int32.
	require.NoError(t, err)

	pid, err := parent.PpidWithContext(t.Context())
	require.NoError(t, err)

	if pid <= 1 {
		t.Skip("this process has no grandparent to test with")
	}

	return int(pid)
}

func TestRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	dir, err := headers.RuntimeDir()
	require.NoError(t, err)
	assert.Equal(t, "/run/user/1000/toolgate", dir)

	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/home/alice/.cache")
	t.Setenv("HOME", "/home/alice")
	t.Setenv("LocalAppData", "/home/alice/.cache")

	dir, err = headers.RuntimeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/home/alice/.cache", "toolgate", "run"), dir)
}

func TestFindSession(t *testing.T) {
	t.Run("TOOLGATE_SESSION wins over a session file", func(t *testing.T) {
		dir := sessionDir(t)
		writeSession(t, dir, os.Getpid(), "from-file")
		t.Setenv(headers.SessionEnv, "from-env")

		session, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid()})
		require.NoError(t, err)
		assert.Equal(t, "from-env", session)
	})

	t.Run("the file of an ancestor further up is found", func(t *testing.T) {
		dir := sessionDir(t)
		writeSession(t, dir, grandparent(t), "from-grandparent\n")

		session, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid()})
		require.NoError(t, err)
		assert.Equal(t, "from-grandparent", session)
	})

	t.Run("the nearest ancestor with a file wins", func(t *testing.T) {
		dir := sessionDir(t)
		writeSession(t, dir, grandparent(t), "from-grandparent")
		writeSession(t, dir, os.Getppid(), "from-parent")

		session, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid()})
		require.NoError(t, err)
		assert.Equal(t, "from-parent", session)
	})

	t.Run("a process that is not an ancestor is ignored", func(t *testing.T) {
		dir := sessionDir(t)

		other := exec.CommandContext(t.Context(), "sleep", "30")
		require.NoError(t, other.Start())
		t.Cleanup(func() {
			_ = other.Process.Kill()
			_ = other.Wait()
		})

		writeSession(t, dir, other.Process.Pid, "not-mine")

		_, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid(), Wait: 200 * time.Millisecond})
		require.ErrorIs(t, err, headers.ErrNoSession)
	})

	t.Run("a file written while waiting is found", func(t *testing.T) {
		dir := sessionDir(t)

		written := make(chan struct{})

		go func() {
			defer close(written)

			time.Sleep(100 * time.Millisecond)
			assert.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getppid())), []byte("late"), 0o600))
		}()

		session, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid(), Wait: time.Minute})
		<-written
		require.NoError(t, err)
		assert.Equal(t, "late", session)
	})

	t.Run("no file within the bound", func(t *testing.T) {
		sessionDir(t)

		start := time.Now()
		_, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid(), Wait: 200 * time.Millisecond})
		require.ErrorIs(t, err, headers.ErrNoSession)
		assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	})

	t.Run("an invalid session ID is refused", func(t *testing.T) {
		dir := sessionDir(t)
		writeSession(t, dir, os.Getpid(), "not valid/")

		_, err := headers.FindSession(t.Context(), headers.SessionOptions{StartPID: os.Getpid()})
		require.ErrorContains(t, err, "invalid session ID")
	})
}

func TestSessionWaitIsFiveSeconds(t *testing.T) {
	assert.Equal(t, 5*time.Second, headers.SessionWait)
}

func gitInit(t *testing.T, dir string, args ...string) {
	t.Helper()

	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.Regexp(t, `^[A-Za-z0-9._-]+$`, hostname, "the expected values below assume a plain hostname")

	tests := []struct {
		name      string
		env       string
		dirName   string
		remote    string
		want      string
		assertion require.ErrorAssertionFunc
	}{
		{name: "TOOLGATE_WORKSPACE is sent as is", env: "ci.runner-7", dirName: "widgets", want: "ci.runner-7"},
		{name: "ssh remote", dirName: "checkout", remote: "git@github.com:acme/widgets.git", want: hostname + "-widgets"},
		{name: "https remote", dirName: "checkout", remote: "https://github.com/acme/gadget.tool", want: hostname + "-gadget.tool"},
		{name: "no remote uses the directory name", dirName: "my project", want: hostname + "-my-project"},
		{name: "characters outside the pattern become dashes", dirName: "café+tea", want: hostname + "-caf--tea"},
		{
			name:    "cut to 128 characters",
			dirName: strings.Repeat("a", 200),
			want:    (hostname + "-" + strings.Repeat("a", 200))[:128],
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(headers.WorkspaceEnv, test.env)

			dir := filepath.Join(t.TempDir(), test.dirName)
			require.NoError(t, os.Mkdir(dir, 0o700))
			gitInit(t, dir, "init", "--quiet")

			if test.remote != "" {
				gitInit(t, dir, "remote", "add", "origin", test.remote)
			}

			if test.assertion == nil {
				test.assertion = require.NoError
			}

			workspace, err := headers.Workspace(t.Context(), dir)
			test.assertion(t, err)
			assert.Equal(t, test.want, workspace)
		})
	}
}
