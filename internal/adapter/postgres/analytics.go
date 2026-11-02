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
// State completion proof is checked once across all headers, including removed markers:
// an old image may advance a header without advancing the still-known state.
const readAnalyticsSQL = `
WITH proven_state AS MATERIALIZED (
    SELECT s.*, CASE WHEN NOT EXISTS (
        SELECT 1 FROM period_snapshot newer
        WHERE newer.user_id = s.user_id AND newer.calc_version > s.calc_version
    ) AND NOT EXISTS (SELECT 1 FROM recalculation_marker pending
        WHERE pending.user_id=s.user_id AND pending.requested_version>s.calc_version)
      THEN s.data_revisions END AS proven_revisions
    FROM user_data_state s WHERE s.user_id = $1
), chosen AS (SELECT coalesce($3::text, (SELECT max(period_key) FROM period_snapshot
    WHERE user_id = $1 AND period_type = $2 AND NOT removed)) AS period_key),
head AS MATERIALIZED (
    SELECT c.period_key, coalesce(s.has_data,false) AS has_data, s.data_from AS state_data_from, s.data_to AS state_data_to, s.last_operation_at,
           s.stale_after, coalesce(s.full_months,0) AS full_months,
           coalesce(s.recalculating,false) OR coalesce(r.requested_version,0)>coalesce(s.calc_version,0) AS recalculating,
           coalesce(p.time_zone,s.time_zone,'') AS time_zone, s.calculated_at AS state_calculated_at,
           p.user_id IS NOT NULL AS has_snapshot,
           p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at,
           g.gap_starts, g.gap_ends,
           (SELECT max(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key < c.period_key AND NOT n.removed) AS previous,
           (SELECT min(n.period_key) FROM period_snapshot n
             WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key > c.period_key AND NOT n.removed) AS next,
           s.proven_revisions AS state_revisions,p.data_revisions AS snapshot_revisions,
           s.calc_version AS state_version,p.calc_version AS snapshot_version,
           (SELECT jsonb_agg(jsonb_build_object(
               'Key', m.period_key, 'Range', jsonb_build_object('Start', m.range_start, 'End', m.range_end),
               'Coverage', m.coverage, 'DataFrom', m.data_from, 'DataTo', m.data_to,
               'Version', m.calc_version, 'Revisions', m.data_revisions,
               'Gaps', coalesce((SELECT jsonb_agg(jsonb_build_object('Start', g.gap_start, 'End', g.gap_end) ORDER BY g.ordinal)
                   FROM period_gap g WHERE g.user_id = m.user_id AND g.period_type = m.period_type
                     AND g.period_key = m.period_key AND g.transfer_mode = m.transfer_mode), '[]'::jsonb)
           ) ORDER BY m.period_key)
           FROM period_snapshot m
           WHERE m.user_id = s.user_id AND m.period_type = 1 AND m.transfer_mode = $4 AND NOT m.removed) AS history,
           s.user_id IS NOT NULL AS has_state,
           p.time_zone AS snapshot_time_zone,
           EXISTS(SELECT 1 FROM period_snapshot d WHERE d.user_id=$1 AND d.period_type=$2 AND d.period_key=c.period_key AND d.transfer_mode=$4 AND d.removed) AS removed
    FROM chosen c LEFT JOIN proven_state s ON true
    LEFT JOIN recalculation_marker r ON r.user_id=$1
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
)
SELECT h.*,
       m.metric_id, m.definition_version, m.status, m.value_minor, m.range_start, m.range_end,
       m.lock_reason, m.lock_required, m.lock_available, m.lock_available_from,
       m.basis_start, m.basis_end, m.basis_full_months,
       m.filter_from, m.filter_to, m.filter_category_id, m.filter_kind, m.filter_transfer_mode,
       m.filter_selection, m.filter_selection_name,
 CASE WHEN $2=4 AND m.metric_id='monthly_chart' AND jsonb_typeof(m.details->'monthly_chart'->'points')='array' THEN jsonb_build_object('monthly_chart',jsonb_build_object('points',
 (SELECT jsonb_agg(peak.point) FROM (SELECT point FROM jsonb_array_elements(m.details->'monthly_chart'->'points') point
 WHERE point->>'expense_minor' ~ '^-?[0-9]+$' ORDER BY (point->>'expense_minor')::numeric DESC,point->>'month' LIMIT 1) peak))) ELSE m.details END
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
	history        []byte
	hasState       bool
	removed        bool
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
			&header.unreadLines, &header.calculatedAt, &gapStarts, &gapEnds, &out.previous, &out.next, &out.state.revisions, &header.revisions, &out.state.version, &header.version, &out.history,
			&out.hasState, &header.timeZone, &out.removed,
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

// ReadAnalytics uses a coherent primary snapshot for state, default selection, data and navigation.
func (r *Metrics) ReadAnalytics(ctx context.Context, user string, typ domain.PeriodType, selected *string, mode domain.TransferMode) (domain.AnalyticsData, error) {
	var out domain.AnalyticsData
	err := r.chartSnapshot(ctx, func(ctx context.Context) error {
		out = domain.AnalyticsData{}
		var err error
		out, err = r.readAnalytics(ctx, user, typ, selected, mode)
		if err != nil || out.State == nil || !out.State.HasData {
			return err
		}
		var anchor *string
		if typ == domain.PeriodMonth {
			anchor = out.SelectedKey
		}
		out.Chart, err = r.chartPage(ctx, user, mode, anchor, "", 0)
		if err != nil {
			return err
		}
		if out.Chart != nil && out.Chart.TimeZone != out.State.TimeZone {
			out.Chart = nil
		}
		return nil
	})
	return out, err
}

func (r *Metrics) readAnalytics(ctx context.Context, user string, typ domain.PeriodType, selected *string, mode domain.TransferMode) (domain.AnalyticsData, error) {
	key := periodKey{typ: int16(typ), mode: int16(mode)}
	if selected != nil {
		key.key = *selected
	}
	data, err := readAnalyticsRows(ctx, r.db.Writer(ctx), user, key, selected)
	if errors.Is(err, errNoState) {
		return domain.AnalyticsData{}, nil
	}
	if err != nil {
		return domain.AnalyticsData{}, err
	}
	out := domain.AnalyticsData{Recalculating: data.state.recalculating, Removed: data.removed}
	if data.state.version != nil {
		out.AppliedVersion = *data.state.version
	}
	if !data.hasState {
		return out, nil
	}
	state := domain.State{HasData: data.state.hasData, DataFrom: data.state.dataFrom, DataTo: data.state.dataTo,
		LastOperationAt: data.state.lastOperation, StaleAfter: data.state.stale, FullMonths: data.state.fullMonths,
		Recalculating: data.state.recalculating, TimeZone: data.state.timeZone, CalculatedAt: data.state.calculatedAt}
	state.Revisions, err = revisionsFromJSON(data.state.revisions, data.state.version)
	if err != nil {
		return domain.AnalyticsData{}, err
	}
	out.State = &state
	out.Snapshot = data.snapshot
	out.Previous = data.previous
	out.Next = data.next
	out.SelectedKey = data.selected
	out.History = forecastHistory(data.history, state, data.snapshot)

	return out, nil
}

func domainSnapshot(key periodKey, h headerRow, starts, ends []time.Time, rows []metricRow) (*domain.Snapshot, error) {
	if h.rangeStart == nil || h.rangeEnd == nil || h.coverage == nil || h.dataFrom == nil || h.dataTo == nil || h.unreadLines == nil || h.calculatedAt == nil || len(starts) != len(ends) {
		return nil, fmt.Errorf("%w: incomplete snapshot", errBrokenHeader)
	}
	revisions, err := revisionsFromJSON(h.revisions, h.version)
	if err != nil {
		return nil, err
	}
	out := &domain.Snapshot{Revisions: revisions, Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodType(key.typ), Key: key.key}, Mode: domain.TransferMode(key.mode)},
		Range: &domain.TimeRange{Start: h.rangeStart, End: h.rangeEnd}, Coverage: domain.Coverage(*h.coverage), DataFrom: h.dataFrom, DataTo: h.dataTo, UnreadLines: *h.unreadLines, CalculatedAt: h.calculatedAt}
	if h.timeZone != nil {
		out.TimeZone = *h.timeZone
	}
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
