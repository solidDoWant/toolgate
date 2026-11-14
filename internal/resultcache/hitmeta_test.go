package resultcache_test

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/resultcache"
)

func TestHitMeta(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		age    time.Duration
		want   mcp.Meta
	}{
		{name: "age is set and cut to whole seconds",
			stored: `{"content":[{"type":"text","text":"ok"}]}`, age: 12*time.Minute + 999*time.Millisecond,
			want: mcp.Meta{"toolgate/cache": map[string]any{"age": "12m0s"}}},
		{name: "an age under a second is 0s",
			stored: `{"content":[]}`, age: 400 * time.Millisecond,
			want: mcp.Meta{"toolgate/cache": map[string]any{"age": "0s"}}},
		{name: "the stored _meta keys stay",
			stored: `{"content":[],"_meta":{"vendor/trace":"abc","count":2}}`, age: 3 * time.Second,
			want: mcp.Meta{"vendor/trace": "abc", "count": float64(2), "toolgate/cache": map[string]any{"age": "3s"}}},
		{name: "a server's own toolgate/cache is replaced",
			stored: `{"content":[],"_meta":{"toolgate/cache":{"ttl":"1h","age":"99h"}}}`, age: 90 * time.Second,
			want: mcp.Meta{"toolgate/cache": map[string]any{"age": "1m30s"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := resultcache.HitResult(resultcache.Hit{Result: []byte(test.stored), Age: test.age})
			require.NoError(t, err)
			assert.Equal(t, test.want, result.Meta)
		})
	}

	t.Run("the content is the stored content", func(t *testing.T) {
		result, err := resultcache.HitResult(resultcache.Hit{Result: []byte(`{"content":[{"type":"text","text":"10k resistor"}],"isError":false}`)})
		require.NoError(t, err)
		require.Len(t, result.Content, 1)
		assert.Equal(t, "10k resistor", result.Content[0].(*mcp.TextContent).Text)
	})

	t.Run("a stored result that doesn't decode is an error", func(t *testing.T) {
		_, err := resultcache.HitResult(resultcache.Hit{Result: []byte(`[`)})
		require.Error(t, err)
	})
}
