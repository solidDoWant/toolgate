package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "dev\n"},
		{name: "no command", args: nil, wantCode: 2, wantStderr: "Usage: toolgate"},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: 2, wantStderr: `unknown command "frobnicate"`},
		{name: "serve is a stub", args: []string{"serve"}, wantCode: 1, wantStderr: "not implemented yet"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage: toolgate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(test.args, &stdout, &stderr)

			require.Equal(t, test.wantCode, code, "stderr: %s", stderr.String())
			assert.Contains(t, stdout.String(), test.wantStdout)
			assert.Contains(t, stderr.String(), test.wantStderr)
		})
	}
}
