package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// policyState loads the grants and temporary blocks one request decides
// with (docs/design/phase-2.md §3, "Loading grants and blocks"). There is no
// cache of rows: a revoke on another replica refuses the next decision here
// at once.
func (m *metaTools) policyState(ctx context.Context, identity auth.Identity) (policy.State, error) {
	rows, err := store.LoadPolicyState(ctx, m.database, identity.Principal, identity.Attestation != auth.AttestationAttested)
	if err != nil {
		return policy.State{}, err
	}

	state := policy.State{Grants: make([]policy.Grant, 0, len(rows.Grants))}

	for _, grant := range rows.Grants {
		state.Grants = append(state.Grants, policy.Grant{
			ID:                grant.ID,
			Principal:         grant.Principal,
			TemplateNamespace: grant.TemplateNs,
			TemplateName:      grant.TemplateName,
			Tool:              grant.Tool,
			Session:           grant.SessionID.String,
		})
	}

	state.Blocks, err = m.blocks.compile(rows.Blocks)
	if err != nil {
		return policy.State{}, err
	}

	return state, nil
}

// blockCache holds compiled temporary blocks by ID. A block's row is never
// changed after insert, so a compiled block can't go stale. Blocks no
// longer live are dropped on the next load.
type blockCache struct {
	mu       sync.Mutex
	compiled map[string]*policy.TemporaryBlock
}

// compile returns the compiled blocks of rows, in the same order.
func (c *blockCache) compile(rows []db.TemporaryBlock) ([]*policy.TemporaryBlock, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.compiled == nil {
		c.compiled = make(map[string]*policy.TemporaryBlock)
	}

	live := make(map[string]*policy.TemporaryBlock, len(rows))
	blocks := make([]*policy.TemporaryBlock, 0, len(rows))

	for _, row := range rows {
		compiled, found := c.compiled[row.ID]
		if !found {
			var match v1alpha1.RuleMatch
			if err := json.Unmarshal(row.Match, &match); err != nil {
				return nil, fmt.Errorf("decode match of temporary block %s: %w", row.ID, err)
			}

			var err error

			compiled, err = policy.NewTemporaryBlock(row.ID, match, row.Reason, row.ExpiresAt.Time)
			if err != nil {
				return nil, err
			}
		}

		live[row.ID] = compiled
		blocks = append(blocks, compiled)
	}

	c.compiled = live

	return blocks, nil
}
