package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// RebuildToolSearch rebuilds the tool search rows of one template inside the
// caller's transaction. It takes the template's
// advisory lock first, so two rebuilds of one template run in turn. Then it
// deletes the template's rows and inserts one row for each curated tool of
// its catalog list, and notifies `toolsearch:<uid>`.
//
// template is the last valid generation of the template, or nil when the
// template is deleted or has no valid generation; its rows are then deleted.
// A template with no catalog list has no rows either.
func RebuildToolSearch(ctx context.Context, tx pgx.Tx, uid string, template *v1alpha1.MCPServerTemplate) error {
	return RebuildToolSearchEmbedded(ctx, tx, uid, template, nil, nil)
}

// RebuildToolSearchEmbedded is RebuildToolSearch with vectors: when embedder
// is not nil, it embeds each row's document text and stores the vector with
// the model name. If the endpoint fails, the rows keep an empty vector, the
// failure is counted and logged, and the rebuild still succeeds.
func RebuildToolSearchEmbedded(ctx context.Context, tx pgx.Tx, uid string, template *v1alpha1.MCPServerTemplate, embedder *embeddings.Client, log *slog.Logger) error {
	queries := db.New(tx)

	if err := queries.LockToolSearch(ctx, uid); err != nil {
		return fmt.Errorf("lock tool search of template %s: %w", uid, err)
	}

	if err := queries.DeleteToolSearchForTemplate(ctx, uid); err != nil {
		return fmt.Errorf("delete tool search of template %s: %w", uid, err)
	}

	if template != nil {
		if err := insertToolSearch(ctx, queries, uid, template, embedder, log); err != nil {
			return err
		}
	}

	return store.Notify(ctx, tx, store.KindToolSearch, uid)
}

func insertToolSearch(ctx context.Context, queries *db.Queries, uid string, template *v1alpha1.MCPServerTemplate, embedder *embeddings.Client, log *slog.Logger) error {
	rows, err := queries.ListToolCatalogForTemplate(ctx, uid)
	if err != nil {
		return fmt.Errorf("list tool catalog of template %s: %w", uid, err)
	}

	learned := make([]toolcatalog.Learned, 0, len(rows))
	for _, row := range rows {
		learned = append(learned, toolcatalog.Learned{Generation: row.Generation, Tools: row.Tools})
	}

	upstream, generation, found, err := toolcatalog.CatalogListGeneration(template, learned)
	if err != nil {
		return fmt.Errorf("catalog list of template %s: %w", uid, err)
	}

	if !found {
		return nil
	}

	curated := toolcatalog.Curate(template, upstream)
	partsOf := make([]toolcatalog.SearchParts, len(curated))
	vectors := make([][]float32, len(curated))

	for index, tool := range curated {
		partsOf[index] = toolcatalog.SearchPartsOf(template, tool)
	}

	model := ""

	if embedder != nil && len(curated) > 0 {
		texts := make([]string, len(curated))
		for index := range partsOf {
			texts[index] = partsOf[index].EmbeddingText()
		}

		embedded, err := embedder.EmbedFor(ctx, embeddings.OperationDocument, texts)
		if err != nil {
			embedder.CountError("document", err)

			if log != nil {
				log.WarnContext(ctx, "embedding the tool search rows failed", slog.String("template", uid), slog.Any("error", err))
			}
		} else {
			vectors, model = embedded, embedder.Model()
		}
	}

	for index, tool := range curated {
		parts := partsOf[index]

		if err := queries.InsertToolSearch(ctx, db.InsertToolSearchParams{
			TemplateUid: uid, TemplateNs: template.Namespace, TemplateName: template.Name,
			SourceGeneration: generation, UpstreamName: tool.UpstreamName, ExposedName: tool.ExposedName,
			Embedding: embeddings.FormatVector(vectors[index]), EmbeddingModel: model, NamePart: parts.Name, DescriptionPart: parts.Description, ServerPart: parts.Server, InputPart: parts.Input,
		}); err != nil {
			return fmt.Errorf("insert tool search row %s of template %s: %w", tool.UpstreamName, uid, err)
		}
	}

	return nil
}

// SearchRebuilder keeps the search rows current from template changes. It
// runs on the leader only.
type SearchRebuilder struct {
	Pool *pgxpool.Pool
	// Fence is checked before each write: rows are written only while
	// this replica holds the Lease.
	Fence Fence
	// Templates returns the last valid generation of every template that
	// has one, keyed by UID.
	Templates func() map[string]*v1alpha1.MCPServerTemplate

	// Changes receives after any template is set or deleted
	// (catalog.Catalog.Subscribe).
	Changes func() (<-chan struct{}, func())
	// Interval between syncs that retry after a failure. Zero means
	// DefaultPassInterval.
	Interval time.Duration
	// Loaded, when set, is closed once every watched template has been
	// read; Run waits for it before its first sync.
	Loaded <-chan struct{}
	Log    *slog.Logger
	// Embedder embeds the rows' documents; nil means words only.
	Embedder *embeddings.Client

	built map[string]string
}

// Run syncs once, then after each template change and each Interval, until
// ctx ends. Add it to the manager as a runnable that needs leader election,
// so it starts afresh, and rebuilds every template, each time this replica
// gets the Lease.
func (r *SearchRebuilder) Run(ctx context.Context) error {
	r.built = nil

	if r.Loaded != nil {
		select {
		case <-ctx.Done():
			return nil
		case <-r.Loaded:
		}
	}

	changes, unsubscribe := r.Changes()
	defer unsubscribe()

	interval := r.Interval
	if interval == 0 {
		interval = DefaultPassInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := r.Sync(ctx); err != nil && ctx.Err() == nil {
			r.Log.WarnContext(ctx, "tool search rebuild failed", slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-changes:
		case <-ticker.C:
		}
	}
}

// Sync rebuilds the rows of every template whose generation, curation or
// declared tools changed since the last Sync, and deletes the rows of
// templates that have no valid generation any more. The first Sync, after
// the replica gets the Lease, rebuilds every template. It also deletes rows
// of UIDs it has never seen, which an earlier leader left behind.
func (r *SearchRebuilder) Sync(ctx context.Context) error {
	if r.Fence != nil && !r.Fence.HoldsLease() {
		return ErrNotLeader
	}

	first := r.built == nil
	if first {
		r.built = map[string]string{}
	}

	current := r.Templates()

	var stored map[string]bool

	if first {
		uids, err := db.New(r.Pool).ListToolSearchUIDs(ctx)
		if err != nil {
			return fmt.Errorf("list tool search templates: %w", err)
		}

		stored = map[string]bool{}
		for _, uid := range uids {
			stored[uid] = true
		}
	}

	rebuilt := map[string]bool{}

	for uid, template := range current {
		fingerprint := r.fingerprint(template)
		if r.built[uid] == fingerprint {
			continue
		}

		if err := r.rebuild(ctx, uid, template); err != nil {
			return err
		}

		r.built[uid] = fingerprint
		rebuilt[uid] = true
	}

	if err := r.embedStale(ctx, current, rebuilt); err != nil {
		return err
	}

	gone := map[string]bool{}
	for uid := range r.built {
		gone[uid] = true
	}

	for uid := range stored {
		gone[uid] = true
	}

	for uid := range gone {
		if current[uid] != nil {
			continue
		}

		if err := r.rebuild(ctx, uid, nil); err != nil {
			return err
		}

		delete(r.built, uid)
	}

	return nil
}

// embedStale rebuilds the templates that have a row not embedded with the
// configured model: after a model change, after a failed embedding, or when
// a row was written without a vector. A template rebuilt in this Sync is
// left for the next one.
func (r *SearchRebuilder) embedStale(ctx context.Context, current map[string]*v1alpha1.MCPServerTemplate, rebuilt map[string]bool) error {
	if r.Embedder == nil {
		return nil
	}

	// A probe gives the length of the vectors the endpoint returns now. If it
	// fails, the rows are compared by model only.
	dimensions := int32(-1)

	if vectors, probeErr := r.Embedder.EmbedFor(ctx, embeddings.OperationDocument, []string{"dimension probe"}); probeErr == nil && len(vectors) == 1 {
		dimensions = int32(len(vectors[0]))
	}

	uids, err := db.New(r.Pool).ListToolSearchUIDsNeedingEmbedding(ctx, db.ListToolSearchUIDsNeedingEmbeddingParams{
		Model: r.Embedder.Model(), Dimensions: dimensions,
	})
	if err != nil {
		return fmt.Errorf("list tool search rows to embed: %w", err)
	}

	for _, uid := range uids {
		if template := current[uid]; template != nil && !rebuilt[uid] {
			if err := r.rebuild(ctx, uid, template); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *SearchRebuilder) fingerprint(template *v1alpha1.MCPServerTemplate) string {
	if r.Embedder == nil {
		return searchFingerprint(template)
	}

	return searchFingerprint(template) + "|" + r.Embedder.Model()
}

func (r *SearchRebuilder) rebuild(ctx context.Context, uid string, template *v1alpha1.MCPServerTemplate) error {
	if r.Fence != nil && !r.Fence.HoldsLease() {
		return ErrNotLeader
	}

	if err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		return RebuildToolSearchEmbedded(ctx, tx, uid, template, r.Embedder, r.Log)
	}); err != nil {
		return fmt.Errorf("rebuild tool search of template %s: %w", uid, err)
	}

	return nil
}

// searchFingerprint covers what a template event can change in the rows: the
// generation, the curation and the declared tools, plus the text of the
// document that comes from the template.
func searchFingerprint(template *v1alpha1.MCPServerTemplate) string {
	encoded, err := json.Marshal([]any{template.Generation, template.Name, template.Spec.Description,
		template.Spec.ToolFilter, template.Spec.Tools, template.Spec.DeclaredTools})
	if err != nil {
		// Rebuild on every sync rather than miss a change.
		return fmt.Sprintf("unencodable-%p", template)
	}

	return string(encoded)
}
