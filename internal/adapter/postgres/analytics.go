package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

// readAnalyticsSQL is the one read of GetAnalytics for an exact period key: the state, the
// header of the snapshot in the requested transfer mode with its gaps, the neighbours with data,
// and the metrics — one row per metric, the head repeated. No row: the user has no state yet.
// has_snapshot false: no snapshot for this key and mode (one row, metric columns NULL).
// The head is MATERIALIZED so that the neighbours are looked up once, not per metric row.
const readAnalyticsSQL = `
WITH chosen AS (SELECT coalesce($3::text, (SELECT max(period_key) FROM period_snapshot
    WHERE user_id = $1 AND period_type = $2 AND NOT removed)) AS period_key),
head AS MATERIALIZED (
    SELECT c.period_key, s.has_data, s.data_from AS state_data_from, s.data_to AS state_data_to, s.last_operation_at,
           s.stale_after, s.full_months, s.recalculating, s.time_zone, s.calculated_at AS state_calculated_at,
           p.user_id IS NOT NULL AS has_snapshot,
           p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at,
           g.gap_starts, g.gap_ends,
           (SELECT max(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key < c.period_key AND NOT n.removed) AS previous,
           (SELECT min(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key > c.period_key AND NOT n.removed) AS next
    FROM user_data_state s CROSS JOIN chosen c
    LEFT JOIN period_snapshot p
           ON p.user_id = s.user_id AND p.period_type = $2 AND p.period_key = c.period_key AND p.transfer_mode = $4
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
       ON m.user_id = $1 AND m.period_type = $2 AND m.period_key = h.period_key AND m.transfer_mode = $4
ORDER BY m.metric_id`

// analyticsRead is the result of readAnalyticsSQL.
type analyticsRead struct {
	state          stateRow
	snapshot       *domain.Snapshot
	selected       *string
	previous, next *string
}

// errNoState is readAnalyticsSQL without a row: the user has no state (has_data = false).
var errNoState = errors.New("no state")

func readAnalyticsRows(ctx context.Context, q querier, user string, key periodKey, selected *string) (analyticsRead, error) {
	rows, err := q.Query(ctx, readAnalyticsSQL, user, key.typ, selected, key.mode)
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
			&out.selected, &out.state.hasData, &out.state.dataFrom, &out.state.dataTo, &out.state.lastOperation,
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
		if out.selected != nil {
			key.key = *out.selected
		}
		if out.snapshot, err = domainSnapshot(key, header, gapStarts, gapEnds, metrics); err != nil {
			return analyticsRead{}, err
		}
	}
	return out, nil
}

// ReadAnalytics uses a single statement snapshot for state, default selection, data and navigation.
func (r *Metrics) ReadAnalytics(ctx context.Context, user string, typ domain.PeriodType, selected *string, mode domain.TransferMode) (domain.AnalyticsData, error) {
	key := periodKey{typ: int16(typ), mode: int16(mode)}
	if selected != nil {
		key.key = *selected
	}
	data, err := readAnalyticsRows(ctx, r.db.Reader(ctx), user, key, selected)
	if errors.Is(err, errNoState) {
		return domain.AnalyticsData{}, nil
	}
	if err != nil {
		return domain.AnalyticsData{}, err
	}
	state := domain.State{HasData: data.state.hasData, DataFrom: data.state.dataFrom, DataTo: data.state.dataTo,
		LastOperationAt: data.state.lastOperation, StaleAfter: data.state.stale, FullMonths: data.state.fullMonths,
		Recalculating: data.state.recalculating, TimeZone: data.state.timeZone, CalculatedAt: data.state.calculatedAt}
	return domain.AnalyticsData{State: &state, Snapshot: data.snapshot, Previous: data.previous, Next: data.next, SelectedKey: data.selected}, nil
}

func domainSnapshot(key periodKey, h headerRow, starts, ends []time.Time, rows []metricRow) (*domain.Snapshot, error) {
	if h.rangeStart == nil || h.rangeEnd == nil || h.coverage == nil || h.dataFrom == nil || h.dataTo == nil || h.unreadLines == nil || h.calculatedAt == nil || len(starts) != len(ends) {
		return nil, fmt.Errorf("%w: incomplete snapshot", errBrokenHeader)
	}
	out := &domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodType(key.typ), Key: key.key}, Mode: domain.TransferMode(key.mode)},
		Range: &domain.TimeRange{Start: h.rangeStart, End: h.rangeEnd}, Coverage: domain.Coverage(*h.coverage), DataFrom: h.dataFrom, DataTo: h.dataTo, UnreadLines: *h.unreadLines, CalculatedAt: h.calculatedAt}
	for i := range starts {
		out.Gaps = append(out.Gaps, domain.TimeRange{Start: &starts[i], End: &ends[i]})
	}
	for _, row := range rows {
		m := domain.Metric{ID: row.metricID, DefinitionVersion: row.definitionVersion, Status: int32(row.status), ValueMinor: row.valueMinor, Details: row.details}
		if row.rangeStart != nil || row.rangeEnd != nil {
			m.Range = &domain.TimeRange{Start: row.rangeStart, End: row.rangeEnd}
		}
		if row.lockReason != nil {
			m.Lock = &domain.Lock{Reason: int32(*row.lockReason), Required: row.lockRequired, Available: row.lockAvailable, AvailableFrom: row.lockAvailableFrom}
		}
		if row.basisFullMonths != nil {
			m.Basis = &domain.Basis{FullMonths: *row.basisFullMonths}
			if row.basisStart != nil || row.basisEnd != nil {
				m.Basis.Range = &domain.TimeRange{Start: row.basisStart, End: row.basisEnd}
			}
		}
		if row.filterFrom != nil {
			m.Filter = &domain.Filter{From: row.filterFrom, To: row.filterTo, CategoryID: row.filterCategoryID, Kind: int32(enumInt(row.filterKind)), TransferMode: int32(enumInt(row.filterTransferMode)), Selection: row.filterSelection, SelectionName: row.filterSelectionName}
		}
		out.Metrics = append(out.Metrics, m)
	}
	return out, nil
}
func enumInt(n *int16) int16 {
	if n == nil {
		return 0
	}
	return *n
}
