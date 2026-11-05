package upstreamlimit_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
)

// serve answers every request with size bytes of contentType; the size
// comes from the request's `n` query parameter.
func serve(t *testing.T, contentType string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(bytes.Repeat([]byte("a"), size))
	}))
	t.Cleanup(server.Close)

	return server
}

func get(t *testing.T, client *http.Client, request *http.Request) ([]byte, error) {
	t.Helper()

	response, err := client.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	return io.ReadAll(response.Body)
}

func TestTransportBoundsEachResponseWithoutBudget(t *testing.T) {
	const limit = 1000

	server := serve(t, "application/json")
	client := upstreamlimit.NewClient(server.Client(), limit)

	tests := []struct {
		name        string
		size        int
		errorAssert require.ErrorAssertionFunc
	}{
		{name: "exactly the limit", size: limit},
		{name: "one byte over", size: limit + 1, errorAssert: func(t require.TestingT, err error, _ ...any) {
			require.ErrorIs(t, err, upstreamlimit.ErrTooLarge)
		}},
		{name: "far over", size: 100 * limit, errorAssert: func(t require.TestingT, err error, _ ...any) {
			require.ErrorIs(t, err, upstreamlimit.ErrTooLarge)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errorAssert == nil {
				test.errorAssert = require.NoError
			}

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"?n="+strconv.Itoa(test.size), nil)
			require.NoError(t, err)

			body, err := get(t, client, request)
			test.errorAssert(t, err)
			assert.LessOrEqual(t, len(body), limit+1)

			if err == nil {
				assert.Len(t, body, test.size)
			}
		})
	}
}

func TestTransportChargesContextBudgetAcrossResponses(t *testing.T) {
	server := serve(t, "application/json")
	client := upstreamlimit.NewClient(server.Client(), 1<<20)

	ctx, budget := upstreamlimit.WithBudget(t.Context(), 1000)

	first, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?n=600", nil)
	require.NoError(t, err)

	_, err = get(t, client, first)
	require.NoError(t, err)
	assert.False(t, budget.Exceeded())

	second, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?n=400", nil)
	require.NoError(t, err)

	_, err = get(t, client, second)
	require.NoError(t, err, "600 + 400 bytes is exactly the budget")
	assert.False(t, budget.Exceeded())

	third, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?n=1", nil)
	require.NoError(t, err)

	_, err = get(t, client, third)
	require.ErrorIs(t, err, upstreamlimit.ErrTooLarge)
	assert.True(t, budget.Exceeded())
}

func TestTransportLeavesEventStreamsToTheSDK(t *testing.T) {
	server := serve(t, "text/event-stream; charset=utf-8")
	client := upstreamlimit.NewClient(server.Client(), 10)

	ctx, budget := upstreamlimit.WithBudget(t.Context(), 10)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?n=1000", nil)
	require.NoError(t, err)

	body, err := get(t, client, request)
	require.NoError(t, err)
	assert.Len(t, body, 1000)
	assert.False(t, budget.Exceeded())
}
