package postgres

// Independent protobuf round-trip assertions over the production GetAnalytics SQL and
// row decoder. The production repository returns domain snapshots; this mapper checks
// that every originally accepted protobuf field survives the stored row representation.

import (
	"context"
	"fmt"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Details are stored as protojson of a Metric holding only the oneof: the key names the variant.
// Enum numbers, as in the columns. Unknown keys are discarded on read: a row written by a newer
// release survives a rollback without failing the read.
var detailsUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

func timestampOf(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func enumOf[E ~int32](n *int16) E {
	if n == nil {
		return 0
	}
	return E(*n)
}

// snapshotOf builds the PeriodSnapshot back from its header, gaps and metrics (metrics in
// metric_id order).
func snapshotOf(key periodKey, h headerRow, gapStarts, gapEnds []time.Time, metrics []metricRow) (*analyticsv1.PeriodSnapshot, error) {
	// The schema does not tie removed = false to the data columns (no CHECK, decision of
	// 19.10.2026): a header broken by hand is an error of the read, not a panic or a snapshot
	// with silently empty fields.
	for _, c := range []struct {
		column  string
		missing bool
	}{
		{"range_start", h.rangeStart == nil},
		{"range_end", h.rangeEnd == nil},
		{"coverage", h.coverage == nil},
		{"data_from", h.dataFrom == nil},
		{"data_to", h.dataTo == nil},
		{"unread_lines_count", h.unreadLines == nil},
		{"calculated_at", h.calculatedAt == nil},
	} {
		if c.missing {
			return nil, fmt.Errorf("%w: snapshot %v: %s is NULL", errBrokenHeader, key, c.column)
		}
	}
	s := &analyticsv1.PeriodSnapshot{
		Period:       commonv1.PeriodType(key.typ),
		PeriodKey:    key.key,
		TransferMode: commonv1.TransferMode(key.mode),
		Range:        &commonv1.TimeRange{Start: timestampOf(h.rangeStart), End: timestampOf(h.rangeEnd)},
		Bounds: &commonv1.DataBounds{
			Coverage: enumOf[commonv1.Coverage](h.coverage),
			DataFrom: timestampOf(h.dataFrom),
			DataTo:   timestampOf(h.dataTo),
		},
		UnreadLinesCount: *h.unreadLines,
		CalculatedAt:     timestampOf(h.calculatedAt),
	}
	for i := range gapStarts {
		s.Bounds.Gaps = append(s.Bounds.Gaps, &commonv1.TimeRange{
			Start: timestamppb.New(gapStarts[i]), End: timestamppb.New(gapEnds[i]),
		})
	}
	for _, r := range metrics {
		m := &analyticsv1.Metric{Status: analyticsv1.MetricStatus(r.status), ValueMinor: r.valueMinor}
		if r.rangeStart != nil || r.rangeEnd != nil {
			m.Range = &commonv1.TimeRange{Start: timestampOf(r.rangeStart), End: timestampOf(r.rangeEnd)}
		}
		if r.lockReason != nil {
			m.Lock = &analyticsv1.MetricLock{
				Reason:        analyticsv1.LockReason(*r.lockReason),
				Required:      r.lockRequired,
				Available:     r.lockAvailable,
				AvailableFrom: timestampOf(r.lockAvailableFrom),
			}
		}
		if r.basisFullMonths != nil {
			b := &analyticsv1.MetricBasis{FullMonths: *r.basisFullMonths}
			if r.basisStart != nil || r.basisEnd != nil {
				b.Range = &commonv1.TimeRange{Start: timestampOf(r.basisStart), End: timestampOf(r.basisEnd)}
			}
			m.Basis = b
		}
		if r.filterFrom != nil {
			m.Filters = &commonv1.OperationsFilter{
				From:          timestampOf(r.filterFrom),
				To:            timestampOf(r.filterTo),
				CategoryId:    r.filterCategoryID,
				Kind:          enumOf[commonv1.KindFilter](r.filterKind),
				TransferMode:  enumOf[commonv1.TransferMode](r.filterTransferMode),
				Selection:     r.filterSelection,
				SelectionName: r.filterSelectionName,
			}
		}
		if r.details != nil {
			holder := &analyticsv1.Metric{}
			if err := detailsUnmarshal.Unmarshal(r.details, holder); err != nil {
				return nil, fmt.Errorf("details of %s: %w", r.metricID, err)
			}
			m.Details = holder.Details
		}
		s.Metrics = append(s.Metrics, &analyticsv1.MetricSnapshot{
			MetricId: r.metricID, DefinitionVersion: r.definitionVersion, Metric: m,
		})
	}
	return s, nil
}

// readAnalytics retains the protobuf round-trip assertion of the original schema tests.
func readAnalytics(ctx context.Context, q querier, user string, key periodKey) (testAnalyticsRead, error) {
	data, err := readAnalyticsRows(ctx, q, user, key, &key.key)
	if err != nil {
		return testAnalyticsRead{}, err
	}
	out := testAnalyticsRead{state: data.state, previous: data.previous, next: data.next}
	if data.snapshot != nil {
		s := data.snapshot
		h, e := headerOf(*s)
		if e != nil {
			return out, e
		}
		var starts, ends []time.Time
		for _, g := range s.Gaps {
			starts = append(starts, *g.Start)
			ends = append(ends, *g.End)
		}
		var rows []metricRow
		for _, m := range s.Metrics {
			r, e := metricOf(key, m)
			if e != nil {
				return out, e
			}
			rows = append(rows, r)
		}
		out.snapshot, err = snapshotOf(key, h, starts, ends, rows)
	}
	return out, err
}

type testAnalyticsRead struct {
	state          stateRow
	snapshot       *analyticsv1.PeriodSnapshot
	previous, next *string
}
