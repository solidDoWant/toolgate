package login

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeGatewayURL(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "bare host", input: "toolgate.example.com", want: "https://toolgate.example.com"},
		{name: "host and port", input: "toolgate.example.com:8443", want: "https://toolgate.example.com:8443"},
		{name: "host with trailing slash", input: "toolgate.example.com/", want: "https://toolgate.example.com"},
		{name: "https url unchanged", input: "https://toolgate.example.com", want: "https://toolgate.example.com"},
		{name: "https url with trailing slash", input: "https://toolgate.example.com/", want: "https://toolgate.example.com"},
		{name: "http localhost with port", input: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "http loopback address", input: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "http ipv6 loopback", input: "http://[::1]:8080", want: "http://[::1]:8080"},
		{name: "ipv6 literal with port", input: "[::1]:8443", want: "https://[::1]:8443"},
		{name: "ipv6 literal without port", input: "[2001:db8::1]", want: "https://[2001:db8::1]"},
		{name: "uppercase host is lowered", input: "Toolgate.Example.COM:8443", want: "https://toolgate.example.com:8443"},
		{name: "uppercase scheme and host", input: "HTTPS://Toolgate.Example.COM", want: "https://toolgate.example.com"},
		{name: "whitespace is trimmed", input: "  toolgate.example.com \n", want: "https://toolgate.example.com"},
		{name: "http for a remote host", input: "http://toolgate.example.com", assertErr: require.Error},
		{name: "empty", input: "  ", assertErr: require.Error},
		{name: "path", input: "toolgate.example.com/mcp", assertErr: require.Error},
		{name: "path on a url", input: "https://toolgate.example.com/api/", assertErr: require.Error},
		{name: "query", input: "https://toolgate.example.com?a=b", assertErr: require.Error},
		{name: "empty query", input: "https://toolgate.example.com/?", assertErr: require.Error},
		{name: "fragment", input: "https://toolgate.example.com#x", assertErr: require.Error},
		{name: "user info", input: "https://user:pass@toolgate.example.com", assertErr: require.Error},
		{name: "other scheme", input: "ftp://toolgate.example.com", assertErr: require.Error},
		{name: "no host", input: "https://", assertErr: require.Error},
		{name: "bad port", input: "toolgate.example.com:port", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := NormalizeGatewayURL(test.input)
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
