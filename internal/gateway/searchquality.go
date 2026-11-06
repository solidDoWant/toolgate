package gateway

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// searchRecord is what one toolgate_find_tools call leaves for the search
// quality metrics. It never holds the query text.
type searchRecord struct {
	id       uuid.UUID
	degraded bool
	// found are the returned tools in rank order.
	found []db.SearchToolsRow
}

func newSearchRecord(degraded bool, found []db.SearchToolsRow) (searchRecord, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return searchRecord{}, fmt.Errorf("new search id: %w", err)
	}

	return searchRecord{id: id, degraded: degraded, found: found}, nil
}

func (r searchRecord) pgID() pgtype.UUID { return pgtype.UUID{Bytes: r.id, Valid: true} }

// embeddingModel is the configured embedding model, empty when none is.
func (m *metaTools) embeddingModel() string {
	if m.embedder == nil {
		return ""
	}

	return m.embedder.Model()
}

// recordSearch writes tool.search, points the session at the search, marks
// the found tools with its id and ranks, and replaces the session's open
// search. It returns true when the replaced search had no hit, which is a
// re-search. It runs in the transaction of queries, which holds the session
// row lock.
func (m *metaTools) recordSearch(ctx context.Context, queries *db.Queries, current requestContext, search searchRecord) (researched *db.TakeOpenSearchRow, err error) {
	tools := make([]map[string]any, len(search.found))
	for index, row := range search.found {
		tools[index] = map[string]any{"server": row.TemplateName, "tool": row.UpstreamName, "rank": index + 1}
	}

	event := auditEvent(current, "tool.search", "", map[string]any{
		"search_id": search.id.String(), "tools": tools, "count": len(tools),
		"degraded": search.degraded, "embedding_model": m.embeddingModel(),
	})
	if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, event); err != nil {
		return nil, err
	}

	for index, row := range search.found {
		err := queries.SetSessionFoundToolSearch(ctx, db.SetSessionFoundToolSearchParams{
			SessionPk: current.session.ID, TemplateNs: row.TemplateNs, TemplateName: row.TemplateName, UpstreamName: row.UpstreamName,
			SearchID: search.pgID(), Rank: pgtype.Int4{Int32: int32(index + 1), Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("mark a found tool: %w", err)
		}
	}

	if err := queries.SetSessionLastSearch(ctx, db.SetSessionLastSearchParams{ID: current.session.ID, SearchID: search.pgID()}); err != nil {
		return nil, fmt.Errorf("set the last search: %w", err)
	}

	previous, err := queries.TakeOpenSearch(ctx, current.session.ID)

	switch {
	case err == nil:
		researched = &previous
	case err != pgx.ErrNoRows:
		return nil, fmt.Errorf("take the open search: %w", err)
	}

	err = queries.InsertOpenSearch(ctx, db.InsertOpenSearchParams{
		SessionPk: current.session.ID, SearchID: search.pgID(), Degraded: search.degraded, EmbeddingModel: m.embeddingModel(),
	})
	if err != nil {
		return nil, fmt.Errorf("write the open search: %w", err)
	}

	return researched, nil
}

// recordSearchHit writes tool.search_hit when the call is of a tool in the
// session's latest search that has no hit yet. It runs before the call's
// decision, whatever the policy outcome, in one transaction with the hit
// itself.
func (m *metaTools) recordSearchHit(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, tool string) error {
	if !current.searchMode || current.session.ID == 0 {
		return nil
	}

	var hit db.TakeSearchHitRow

	found := false

	err := pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.LockSession(ctx, current.session.ID); err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		row, err := queries.TakeSearchHit(ctx, db.TakeSearchHitParams{
			SessionPk: current.session.ID, TemplateNs: template.Namespace, TemplateName: template.Name, UpstreamName: tool,
		})
		if err == pgx.ErrNoRows {
			return nil
		}

		if err != nil {
			return fmt.Errorf("take the search hit: %w", err)
		}

		hit, found = row, true

		event := callEvent(current, "tool.search_hit", template.Name, tool, map[string]any{
			"search_id": uuid.UUID(row.SearchID.Bytes).String(), "rank": row.Rank,
			"degraded": row.Degraded, "embedding_model": row.EmbeddingModel,
		})

		return audit.NewRecorder(m.auditOutput, queries).Record(ctx, event)
	})
	if err != nil {
		return fmt.Errorf("The call wasn't forwarded: the gateway can't record it. Try again later. (%w)", err) //nolint:staticcheck // A sentence for the model.
	}

	if found {
		m.metrics.ObserveSearchHit(int(hit.Rank), hit.Degraded, hit.EmbeddingModel)
	}

	return nil
}
