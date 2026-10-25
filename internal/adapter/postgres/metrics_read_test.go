package postgres

// The read of GetAnalytics proposed by docs/schema.md («Чтение»): the one statement for an
// exact period, the default period and the rows → PeriodSnapshot mapping. GetAnalytics is
// FIN-5; until it is implemented the read lives here, next to the production write it is
// checked against: the tests write through the repository and read back through it, so the
// round trip PeriodSnapshot → rows → PeriodSnapshot is what they check.

import (
	"context"
	"errors"
	"fmt"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// readAnalyticsSQL is the one read of GetAnalytics for an exact period key: the state, the
// header of the snapshot in the requested transfer mode with its gaps, the neighbours with data,
// and the metrics — one row per metric, the head repeated. No row: the user has no state yet.
// has_snapshot false: no snapshot for this key and mode (one row, metric columns NULL).
// The head is MATERIALIZED so that the neighbours are looked up once, not per metric row.
const readAnalyticsSQL = `
WITH head AS MATERIALIZED (
    SELECT s.has_data, s.data_from AS state_data_from, s.data_to AS state_data_to, s.last_operation_at,
           s.stale_after, s.full_months, s.recalculating, s.time_zone, s.calculated_at AS state_calculated_at,
           p.user_id IS NOT NULL AS has_snapshot,
           p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at,
           g.gap_starts, g.gap_ends,
           (SELECT max(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key < $3 AND NOT n.removed) AS previous,
           (SELECT min(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key > $3 AND NOT n.removed) AS next
    FROM user_data_state s
    LEFT JOIN period_snapshot p
           ON p.user_id = s.user_id AND p.period_type = $2 AND p.period_key = $3 AND p.transfer_mode = $4
          AND NOT p.removed
    LEFT JOIN LATERAL (
        SELECT array_agg(gp.gap_start ORDER BY gp.ordinal) AS gap_starts,
               array_agg(gp.gap_end ORDER BY gp.ordinal) AS gap_ends
        FROM period_gap gp
        WHERE gp.user_id = p.user_id AND gp.period_type = p.period_type AND gp.period_key = p.period_key
          AND gp.transfer_mode = p.transfer_mode
    ) g ON true
    WHERE s.user_id = $1
)
SELECT h.*,
       m.metric_id, m.definition_version, m.status, m.value_minor, m.range_start, m.range_end,
       m.lock_reason, m.lock_required, m.lock_available, m.lock_available_from,
       m.basis_start, m.basis_end, m.basis_full_months,
       m.filter_from, m.filter_to, m.filter_category_id, m.filter_kind, m.filter_transfer_mode,
       m.filter_selection, m.filter_selection_name, m.details
FROM head h
LEFT JOIN period_metric m
       ON m.user_id = $1 AND m.period_type = $2 AND m.period_key = $3 AND m.transfer_mode = $4
ORDER BY m.metric_id`

// latestPeriodSQL is the default period of GetAnalytics: the latest period of the type with data.
const latestPeriodSQL = `
SELECT max(period_key) FROM period_snapshot
WHERE user_id = $1 AND period_type = $2 AND NOT removed`

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

// analyticsRead is the result of readAnalyticsSQL.
type analyticsRead struct {
	state          stateRow
	snapshot       *analyticsv1.PeriodSnapshot // nil: no snapshot for the key and mode
	previous, next *string
}

// errNoState is readAnalyticsSQL without a row: the user has no state (has_data = false).
var errNoState = errors.New("no state")

func readAnalytics(ctx context.Context, q querier, user string, key periodKey) (analyticsRead, error) {
	rows, err := q.Query(ctx, readAnalyticsSQL, user, key.typ, key.key, key.mode)
	if err != nil {
		return analyticsRead{}, err
	}
	defer rows.Close()
	var (
		out                analyticsRead
		header             headerRow
		hasSnapshot        bool
		gapStarts, gapEnds []time.Time
		metrics            []metricRow
		found              bool
	)
	for rows.Next() {
		found = true
		var m metricRow
		var metricID *string
		var definitionVersion *int32
		var status *int16
		if err := rows.Scan(
			&out.state.hasData, &out.state.dataFrom, &out.state.dataTo, &out.state.lastOperation,
			&out.state.stale, &out.state.fullMonths, &out.state.recalculating, &out.state.timeZone,
			&out.state.calculatedAt,
			&hasSnapshot, &header.rangeStart, &header.rangeEnd, &header.coverage, &header.dataFrom, &header.dataTo,
			&header.unreadLines, &header.calculatedAt, &gapStarts, &gapEnds, &out.previous, &out.next,
			&metricID, &definitionVersion, &status, &m.valueMinor, &m.rangeStart, &m.rangeEnd,
			&m.lockReason, &m.lockRequired, &m.lockAvailable, &m.lockAvailableFrom,
			&m.basisStart, &m.basisEnd, &m.basisFullMonths,
			&m.filterFrom, &m.filterTo, &m.filterCategoryID, &m.filterKind, &m.filterTransferMode,
			&m.filterSelection, &m.filterSelectionName, &m.details,
		); err != nil {
			return analyticsRead{}, err
		}
		if metricID == nil {
			continue // a snapshot without metrics, or no snapshot
		}
		m.metricID, m.definitionVersion, m.status = *metricID, *definitionVersion, *status
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return analyticsRead{}, err
	}
	if !found {
		return analyticsRead{}, errNoState
	}
	if hasSnapshot {
		if out.snapshot, err = snapshotOf(key, header, gapStarts, gapEnds, metrics); err != nil {
			return analyticsRead{}, err
		}
	}
	return out, nil
}
