package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type headerTransport struct{ authorization string }

func (h headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	if h.authorization != "" {
		request.Header.Set("Authorization", h.authorization)
	}

	return http.DefaultTransport.RoundTrip(request)
}

func TestBearerTokenModeRefusesOtherTokensAndNamesTheUser(t *testing.T) {
	handler, err := newHandler(handlerConfig{Tools: "echo,whoami", BearerTokens: "key-a=alice,key-b=bob", Health: "200"})
	require.NoError(t, err)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	connectAs := func(authorization string) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)

		return client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint:   httpServer.URL + "/mcp",
			HTTPClient: &http.Client{Transport: headerTransport{authorization: authorization}},
		}, nil)
	}

	for _, authorization := range []string{"", "Bearer wrong", "Bearer ", "key-a"} {
		_, err := connectAs(authorization)
		assert.Error(t, err, "authorization %q", authorization)
	}

	for token, user := range map[string]string{"key-a": "alice", "key-b": "bob"} {
		session, err := connectAs("Bearer " + token)
		require.NoError(t, err)

		var who map[string]string
		callTool(t, session, "whoami", nil, &who)
		assert.Equal(t, user, who["user"])
		require.NoError(t, session.Close())
	}
}
