package runtime

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// NoCallAfter is how long a search may go without a hit or a newer search
// before it counts as "no call".
const NoCallAfter = 30 * time.Minute

func degradedLabel(degraded bool) string { return strconv.FormatBool(degraded) }

// ObserveSearch counts one tool search. A nil Metrics does nothing.
func (m *Metrics) ObserveSearch(degraded bool, embeddingModel string) {
	if m != nil {
		m.Searches.WithLabelValues(degradedLabel(degraded), embeddingModel).Inc()
	}
}

// ObserveSearchHit counts a call of a tool returned at rank by the latest
// search.
func (m *Metrics) ObserveSearchHit(rank int, degraded bool, embeddingModel string) {
	if m != nil {
		m.SearchHits.WithLabelValues(strconv.Itoa(rank), degradedLabel(degraded), embeddingModel).Inc()
	}
}

// ObserveSearchResearch counts a search replaced by a newer one before any
// hit.
func (m *Metrics) ObserveSearchResearch(degraded bool, embeddingModel string) {
	if m != nil {
		m.SearchResearches.WithLabelValues(degradedLabel(degraded), embeddingModel).Inc()
	}
}

func (m *Metrics) observeSearchNoCall(degraded bool, embeddingModel string) {
	if m != nil {
		m.SearchNoCalls.WithLabelValues(degradedLabel(degraded), embeddingModel).Inc()
	}
}

// settleSearches counts as "no call" every open search of an ended session
// and every one older than NoCallAfter. Each search is deleted as it is
// counted, so it counts once.
func (e *SessionEnder) settleSearches(ctx context.Context) error {
	age := pgtype.Interval{Microseconds: NoCallAfter.Microseconds(), Valid: true}

	rows, err := db.New(e.Pool).TakeStaleOpenSearches(ctx, age)
	if err != nil {
		return fmt.Errorf("settle open searches: %w", err)
	}

	for _, row := range rows {
		e.Metrics.observeSearchNoCall(row.Degraded, row.EmbeddingModel)
	}

	return nil
}
