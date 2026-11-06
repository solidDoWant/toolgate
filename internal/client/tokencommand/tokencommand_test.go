//go:build !windows

package tokencommand

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwtWithExp builds an unsigned JWT-shaped token with the given exp.
func jwtWithExp(exp time.Time) string {
	encode := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }

	return encode(`{"alg":"none"}`) + "." + encode(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + "." + encode("sig")
}

func newSource(t *testing.T, command string) (Source, *bytes.Buffer) {
	t.Helper()

	stderr := &bytes.Buffer{}

	return Source{Command: command, Dir: t.TempDir(), Stderr: stderr}, stderr
}

func TestRunErrors(t *testing.T) {
	tests := map[string]struct {
		command string
		want    string
		stderr  string
	}{
		"exit status":   {command: "echo 'link your account' >&2; exit 3", want: "TOOLGATE_TOKEN_COMMAND exited with status 3.", stderr: "link your account\n"},
		"no token":      {command: "true", want: "TOOLGATE_TOKEN_COMMAND printed no token."},
		"blank":         {command: "echo '   '", want: "TOOLGATE_TOKEN_COMMAND printed no token."},
		"not a token":   {command: "echo 'not a token'", want: "TOOLGATE_TOKEN_COMMAND printed something that isn't a token."},
		"two lines":     {command: "printf 'abc\\ndef\\n'", want: "TOOLGATE_TOKEN_COMMAND printed something that isn't a token."},
		"too big":       {command: "head -c 16385 /dev/zero | tr '\\0' a", want: "TOOLGATE_TOKEN_COMMAND printed more than 16 KiB."},
		"secret output": {command: "echo 'sekret value' ; exit 1", want: "TOOLGATE_TOKEN_COMMAND exited with status 1."},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			source, stderr := newSource(t, test.command)

			_, err := source.Token(t.Context())
			require.Error(t, err)
			assert.Equal(t, test.want, err.Error())
			assert.NotContains(t, err.Error(), "sekret")
			assert.Equal(t, test.stderr, stderr.String())
		})
	}
}

func TestOutputSizeLimitIsExact(t *testing.T) {
	source, _ := newSource(t, "head -c 16384 /dev/zero | tr '\\0' a")

	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Len(t, token, 16384)
}

func TestStderrIsCutAt4KiB(t *testing.T) {
	source, stderr := newSource(t, "head -c 10000 /dev/zero | tr '\\0' e >&2; echo tok")

	_, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Len(t, stderr.String(), 4096)
}

func TestTokenIsTrimmed(t *testing.T) {
	source, _ := newSource(t, "printf '  abc.def-ghi==\\n\\n'")

	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "abc.def-ghi==", token)
}

func TestStdinIsEmpty(t *testing.T) {
	source, _ := newSource(t, "cat; echo tok")

	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok", token)
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	source, _ := newSource(t, "(sleep 2; touch "+marker+") & sleep 30")
	source.Timeout = 300 * time.Millisecond

	started := time.Now()
	_, err := source.Token(t.Context())
	require.Error(t, err)
	assert.Equal(t, "TOOLGATE_TOKEN_COMMAND didn't finish within 0 s.", err.Error())
	assert.Less(t, time.Since(started), 10*time.Second)

	// The background child belongs to the group, so it was killed too.
	assert.Never(t, func() bool { _, err := os.Stat(marker); return err == nil }, 3*time.Second, 100*time.Millisecond)
}

func TestCache(t *testing.T) {
	now := time.Now()
	counter := filepath.Join(t.TempDir(), "runs")
	fresh := jwtWithExp(now.Add(10 * time.Minute))

	source, _ := newSource(t, "echo run >> "+counter+"; echo "+fresh)

	for range 3 {
		token, err := source.Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, fresh, token)
	}

	assert.Equal(t, 1, strings.Count(readFile(t, counter), "run"), "the cached token is reused")

	entries, err := os.ReadDir(source.Dir)
	require.NoError(t, err)

	var cacheFile string

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "token-command-") {
			cacheFile = filepath.Join(source.Dir, entry.Name())
		}
	}

	require.NotEmpty(t, cacheFile)
	assert.Regexp(t, `token-command-[0-9a-f]{16}$`, cacheFile)

	info, err := os.Stat(cacheFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// 61 s before the expiry the token is still handed out; 59 s before, it isn't.
	source.Now = func() time.Time { return now.Add(10*time.Minute - 61*time.Second) }
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(readFile(t, counter), "run"))

	source.Now = func() time.Time { return now.Add(10*time.Minute - 59*time.Second) }
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(readFile(t, counter), "run"))
}

func TestTokenWithoutExpIsNotCached(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	source, _ := newSource(t, "echo run >> "+counter+"; echo opaque-token")

	for range 2 {
		_, err := source.Token(t.Context())
		require.NoError(t, err)
	}

	assert.Equal(t, 2, strings.Count(readFile(t, counter), "run"))
}

func TestInvalidateRunsTheCommandAgain(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	source, _ := newSource(t, "echo run >> "+counter+"; echo "+jwtWithExp(time.Now().Add(time.Hour)))

	_, err := source.Token(t.Context())
	require.NoError(t, err)
	require.NoError(t, source.Invalidate())
	require.NoError(t, source.Invalidate(), "nothing cached is fine")
	_, err = source.Token(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 2, strings.Count(readFile(t, counter), "run"))
}

func TestConcurrentHelpersRunTheCommandOnce(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	source, _ := newSource(t, "echo run >> "+counter+"; sleep 0.3; echo "+jwtWithExp(time.Now().Add(time.Hour)))

	var wait sync.WaitGroup

	for range 2 {
		wait.Add(1)

		go func() {
			defer wait.Done()

			_, err := source.Token(t.Context())
			assert.NoError(t, err)
		}()
	}

	wait.Wait()
	assert.Equal(t, 1, strings.Count(readFile(t, counter), "run"))
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(content)
}
