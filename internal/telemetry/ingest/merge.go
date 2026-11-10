package ingest

import (
	"encoding/json"
	"slices"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// pointKey identifies one series at one moment: two data points of a metric
// with the same key would collide at the backend.
type pointKey struct {
	attributes string
	start, end pcommon.Timestamp
}

func keyOf(attributes pcommon.Map, start, end pcommon.Timestamp) pointKey {
	// encoding/json sorts map keys, so equal attribute sets give equal text.
	encoded, _ := json.Marshal(attributes.AsRaw())

	return pointKey{attributes: string(encoded), start: start, end: end}
}

// mergeDuplicatePoints combines the data points of a metric that became
// identical (same attributes and timestamps) once a label was removed, so the
// backend sees one point and nothing is counted twice or rejected. Sums and
// histogram counts add up; a gauge keeps its last reading. Histograms with
// different bucket bounds, exponential histograms and summaries can't be
// added, so the first point is kept.
func mergeDuplicatePoints(metric pmetric.Metric) {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		mergeNumberPoints(metric.Gauge().DataPoints(), func(kept, duplicate pmetric.NumberDataPoint) {
			duplicate.CopyTo(kept)
		})
	case pmetric.MetricTypeSum:
		mergeNumberPoints(metric.Sum().DataPoints(), addNumbers)
	case pmetric.MetricTypeHistogram:
		points := metric.Histogram().DataPoints()
		seen := map[pointKey]pmetric.HistogramDataPoint{}

		points.RemoveIf(func(point pmetric.HistogramDataPoint) bool {
			key := keyOf(point.Attributes(), point.StartTimestamp(), point.Timestamp())

			kept, found := seen[key]
			if !found {
				seen[key] = point

				return false
			}

			addHistograms(kept, point)

			return true
		})
	case pmetric.MetricTypeExponentialHistogram:
		points := metric.ExponentialHistogram().DataPoints()
		seen := map[pointKey]bool{}

		points.RemoveIf(func(point pmetric.ExponentialHistogramDataPoint) bool {
			key := keyOf(point.Attributes(), point.StartTimestamp(), point.Timestamp())
			defer func() { seen[key] = true }()

			return seen[key]
		})
	case pmetric.MetricTypeSummary:
		points := metric.Summary().DataPoints()
		seen := map[pointKey]bool{}

		points.RemoveIf(func(point pmetric.SummaryDataPoint) bool {
			key := keyOf(point.Attributes(), point.StartTimestamp(), point.Timestamp())
			defer func() { seen[key] = true }()

			return seen[key]
		})
	case pmetric.MetricTypeEmpty:
	}
}

func mergeNumberPoints(points pmetric.NumberDataPointSlice, merge func(kept, duplicate pmetric.NumberDataPoint)) {
	seen := map[pointKey]pmetric.NumberDataPoint{}

	points.RemoveIf(func(point pmetric.NumberDataPoint) bool {
		key := keyOf(point.Attributes(), point.StartTimestamp(), point.Timestamp())

		kept, found := seen[key]
		if !found {
			seen[key] = point

			return false
		}

		merge(kept, point)

		return true
	})
}

func addNumbers(kept, duplicate pmetric.NumberDataPoint) {
	switch {
	case kept.ValueType() == pmetric.NumberDataPointValueTypeInt &&
		duplicate.ValueType() == pmetric.NumberDataPointValueTypeInt:
		kept.SetIntValue(kept.IntValue() + duplicate.IntValue())
	default:
		kept.SetDoubleValue(numberAsDouble(kept) + numberAsDouble(duplicate))
	}
}

func numberAsDouble(point pmetric.NumberDataPoint) float64 {
	if point.ValueType() == pmetric.NumberDataPointValueTypeInt {
		return float64(point.IntValue())
	}

	return point.DoubleValue()
}

func addHistograms(kept, duplicate pmetric.HistogramDataPoint) {
	if !slices.Equal(kept.ExplicitBounds().AsRaw(), duplicate.ExplicitBounds().AsRaw()) ||
		kept.BucketCounts().Len() != duplicate.BucketCounts().Len() {
		return
	}

	kept.SetCount(kept.Count() + duplicate.Count())

	if kept.HasSum() && duplicate.HasSum() {
		kept.SetSum(kept.Sum() + duplicate.Sum())
	} else {
		kept.RemoveSum()
	}

	for index := range kept.BucketCounts().Len() {
		kept.BucketCounts().SetAt(index, kept.BucketCounts().At(index)+duplicate.BucketCounts().At(index))
	}

	switch {
	case kept.HasMin() && duplicate.HasMin():
		kept.SetMin(min(kept.Min(), duplicate.Min()))
	default:
		kept.RemoveMin()
	}

	switch {
	case kept.HasMax() && duplicate.HasMax():
		kept.SetMax(max(kept.Max(), duplicate.Max()))
	default:
		kept.RemoveMax()
	}
}
