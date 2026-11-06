package gateway

import (
	"sort"

	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const (
	// rrfK is the constant of reciprocal rank fusion.
	rrfK = 60
	// maxSearchRows is the number of rows a search returns.
	maxSearchRows = 100
)

// fuseRanks ranks the rows by reciprocal rank fusion of two lists: the rows
// that match the words, best score first, and the rows embedded with model,
// most similar to queryVector first. A row missing from a list gets nothing
// from it. Ties go to the lower exposed name. It returns at most
// maxSearchRows rows.
func fuseRanks(rows []db.SearchToolsWithVectorsRow, queryVector []float32, model string) []db.SearchToolsRow {
	type ranked struct {
		row   db.SearchToolsWithVectorsRow
		score float64
	}

	items := make([]ranked, len(rows))
	for index, row := range rows {
		items[index] = ranked{row: row}
	}

	// The word list.
	words := make([]int, 0, len(items))

	for index, item := range items {
		if item.row.Matched {
			words = append(words, index)
		}
	}

	sort.SliceStable(words, func(a, b int) bool {
		first, second := items[words[a]].row, items[words[b]].row
		if first.Score != second.Score {
			return first.Score > second.Score
		}

		return first.ExposedName < second.ExposedName
	})

	for rank, index := range words {
		items[index].score += 1 / float64(rrfK+rank+1)
	}

	// The vector list.
	similarity := make(map[int]float64, len(items))
	vectors := make([]int, 0, len(items))

	for index, item := range items {
		if item.row.EmbeddingModel == model && len(item.row.Embedding) == len(queryVector) {
			similarity[index] = embeddings.Cosine(item.row.Embedding, queryVector)
			vectors = append(vectors, index)
		}
	}

	sort.SliceStable(vectors, func(a, b int) bool {
		if similarity[vectors[a]] != similarity[vectors[b]] {
			return similarity[vectors[a]] > similarity[vectors[b]]
		}

		return items[vectors[a]].row.ExposedName < items[vectors[b]].row.ExposedName
	})

	for rank, index := range vectors {
		items[index].score += 1 / float64(rrfK+rank+1)
	}

	sort.SliceStable(items, func(a, b int) bool {
		if items[a].score != items[b].score {
			return items[a].score > items[b].score
		}

		return items[a].row.ExposedName < items[b].row.ExposedName
	})

	out := make([]db.SearchToolsRow, 0, min(len(items), maxSearchRows))

	for _, item := range items {
		if item.score == 0 || len(out) >= maxSearchRows {
			break
		}

		out = append(out, db.SearchToolsRow{
			TemplateUid: item.row.TemplateUid, TemplateNs: item.row.TemplateNs, TemplateName: item.row.TemplateName,
			UpstreamName: item.row.UpstreamName, ExposedName: item.row.ExposedName,
		})
	}

	return out
}
