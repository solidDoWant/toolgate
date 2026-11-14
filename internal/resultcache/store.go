package resultcache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Store keeps cached tool results for every replica.
type Store interface {
	// Get returns the unexpired entry for key, with its age.
	Get(ctx context.Context, key Key) (Hit, bool, error)
	// Put stores result for key for ttl. It returns why nothing was stored,
	// or PutStored.
	Put(ctx context.Context, key Key, result []byte, ttl time.Duration) (PutResult, error)
	// DeleteTemplate deletes the entries of a template UID of every
	// generation except keep. keep 0 deletes them all.
	DeleteTemplate(ctx context.Context, templateUID string, keep int64) error
	// Cleanup deletes expired entries, per-session entries of ended
	// sessions, and, through DeleteTemplate, entries of template UIDs and
	// generations that current does not map.
	Cleanup(ctx context.Context, current map[string]int64) error
}

// Hit is a cached result.
type Hit struct {
	// Result is the JSON of the CallToolResult as the server sent it.
	Result []byte
	// Age is how long ago the entry was stored, by the database's clock.
	Age time.Duration
}

// PutResult says whether Put stored the result, and why not.
type PutResult string

// Put results.
const (
	PutStored        PutResult = "stored"
	PutEntryTooLarge PutResult = "entry_too_large"
	PutTemplateFull  PutResult = "template_full"
	PutSessionEnded  PutResult = "session_ended"
)

// HitResult decodes a hit's stored result and sets `_meta["toolgate/cache"]`
// to its age, cut to whole seconds. The stored result's other `_meta` keys
// are kept, and a `toolgate/cache` key the server set is replaced.
func HitResult(hit Hit) (*mcp.CallToolResult, error) {
	result := &mcp.CallToolResult{}
	if err := json.Unmarshal(hit.Result, result); err != nil {
		return nil, fmt.Errorf("decode cached result: %w", err)
	}

	meta := make(mcp.Meta, len(result.Meta)+1)
	for name, value := range result.Meta {
		meta[name] = value
	}

	meta[MetaKey] = map[string]any{"age": hit.Age.Truncate(time.Second).String()}
	result.Meta = meta

	return result, nil
}
