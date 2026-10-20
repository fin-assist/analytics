package migrations_test

// The mapping between PeriodSnapshot and the rows of period_snapshot, period_gap and
// period_metric, and the write and read statements of docs/schema.md («Запись», «Чтение»).
// It is the proposal the repository will take: the tests of schema_test.go write and read
// through it, so the round trip PeriodSnapshot → rows → PeriodSnapshot is what they check.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// upsertHeadersSQL applies the headers of the snapshots and the removal markers of one request
// in one statement. Rows are taken in primary key order, so two requests of the same user lock
// their rows in the same order and cannot deadlock. A key may appear only once per statement
// (ON CONFLICT cannot touch a row twice). RETURNING gives the keys that were applied: only their
// gaps and metrics are replaced.
const upsertHeadersSQL = `
INSERT INTO period_snapshot AS s
    (user_id, period_type, period_key, transfer_mode, calc_version, removed,
     range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at)
SELECT $1, p.period_type, p.period_key, p.transfer_mode, $2, p.removed,
       p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at
FROM unnest($3::smallint[], $4::text[], $5::smallint[], $6::boolean[],
            $7::timestamptz[], $8::timestamptz[], $9::smallint[], $10::timestamptz[], $11::timestamptz[],
            $12::integer[], $13::timestamptz[])
    AS p (period_type, period_key, transfer_mode, removed,
          range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at)
ORDER BY p.period_type, p.period_key COLLATE "C", p.transfer_mode
ON CONFLICT (user_id, period_type, period_key, transfer_mode) DO UPDATE
SET calc_version = EXCLUDED.calc_version,
    removed = EXCLUDED.removed,
    range_start = EXCLUDED.range_start,
    range_end = EXCLUDED.range_end,
    coverage = EXCLUDED.coverage,
    data_from = EXCLUDED.data_from,
    data_to = EXCLUDED.data_to,
    unread_lines_count = EXCLUDED.unread_lines_count,
    calculated_at = EXCLUDED.calculated_at,
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.period_type, s.period_key, s.transfer_mode, s.removed`

// deleteChildrenSQL removes the gaps and the metrics of the applied keys. It is a statement of
// its own, run after upsertHeadersSQL: in READ COMMITTED it takes a new snapshot, so it sees the
// rows of a writer that held the header lock before this one and committed while it waited.
// A data-modifying CTE of the header statement would use the snapshot taken before the wait
// and leave those rows behind.
const deleteChildrenSQL = `
WITH gaps AS (
    DELETE FROM period_gap g
    USING unnest($2::smallint[], $3::text[], $4::smallint[]) AS a (period_type, period_key, transfer_mode)
    WHERE g.user_id = $1 AND g.period_type = a.period_type AND g.period_key = a.period_key
      AND g.transfer_mode = a.transfer_mode
)
DELETE FROM period_metric m
USING unnest($2::smallint[], $3::text[], $4::smallint[]) AS a (period_type, period_key, transfer_mode)
WHERE m.user_id = $1 AND m.period_type = a.period_type AND m.period_key = a.period_key
  AND m.transfer_mode = a.transfer_mode`

// insertGapsSQL writes the gaps of the applied snapshots.
const insertGapsSQL = `
INSERT INTO period_gap (user_id, period_type, period_key, transfer_mode, ordinal, gap_start, gap_end)
SELECT $1, g.period_type, g.period_key, g.transfer_mode, g.ordinal, g.gap_start, g.gap_end
FROM unnest($2::smallint[], $3::text[], $4::smallint[], $5::smallint[], $6::timestamptz[], $7::timestamptz[])
    AS g (period_type, period_key, transfer_mode, ordinal, gap_start, gap_end)`

// insertMetricsSQL writes the metrics of the applied snapshots: the full set of each period.
const insertMetricsSQL = `
INSERT INTO period_metric
    (user_id, period_type, period_key, transfer_mode, metric_id, definition_version, status, value_minor,
     range_start, range_end, lock_reason, lock_required, lock_available, lock_available_from,
     basis_start, basis_end, basis_full_months,
     filter_from, filter_to, filter_category_id, filter_kind, filter_transfer_mode, filter_selection,
     filter_selection_name, details)
SELECT $1, m.*
FROM unnest($2::smallint[], $3::text[], $4::smallint[], $5::text[], $6::integer[], $7::smallint[], $8::bigint[],
            $9::timestamptz[], $10::timestamptz[], $11::smallint[], $12::integer[], $13::integer[], $14::timestamptz[],
            $15::timestamptz[], $16::timestamptz[], $17::integer[],
            $18::timestamptz[], $19::timestamptz[], $20::text[], $21::smallint[], $22::smallint[], $23::text[],
            $24::text[], $25::jsonb[])
    AS m (period_type, period_key, transfer_mode, metric_id, definition_version, status, value_minor,
          range_start, range_end, lock_reason, lock_required, lock_available, lock_available_from,
          basis_start, basis_end, basis_full_months,
          filter_from, filter_to, filter_category_id, filter_kind, filter_transfer_mode, filter_selection,
          filter_selection_name, details)`

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

// listPeriodsSQL is ListPeriods: periods of the type with data, newest first, one row per
// period whatever the transfer mode (ListPeriods has none; coverage does not depend on it).
const listPeriodsSQL = `
SELECT DISTINCT ON (period_key) period_key, range_start, range_end, coverage
FROM period_snapshot
WHERE user_id = $1 AND period_type = $2 AND NOT removed
ORDER BY period_key DESC, transfer_mode DESC`

// purgeSQL is the data part of PurgeUserData, one statement for both tables; gaps and metrics go
// by the cascade of their foreign keys. The tombstone library runs it in its transaction together
// with the tombstone.
const purgeSQL = `
WITH periods AS (DELETE FROM period_snapshot WHERE user_id = $1)
DELETE FROM user_data_state WHERE user_id = $1`

// Details are stored as protojson of a Metric holding only the oneof: the key names the variant.
// Enum numbers, as in the columns. Unknown keys are discarded on read: a row written by a newer
// release survives a rollback without failing the read.
var (
	detailsMarshal   = protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}
	detailsUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// periodKey is the key of a snapshot within a user.
type periodKey struct {
	typ  int16
	key  string
	mode int16
}

// headerRow is a row of period_snapshot: a snapshot header or, with removed, a marker.
type headerRow struct {
	periodKey
	removed          bool
	rangeStart       *time.Time
	rangeEnd         *time.Time
	coverage         *int16
	dataFrom, dataTo *time.Time
	unreadLines      *int32
	calculatedAt     *time.Time
}

type gapRow struct {
	periodKey
	ordinal    int16
	start, end *time.Time // NULL fails NOT NULL: a gap without a bound is not stored as the epoch
}

// metricRow is a row of period_metric.
type metricRow struct {
	periodKey
	metricID            string
	definitionVersion   int32
	status              int16
	valueMinor          *int64
	rangeStart          *time.Time
	rangeEnd            *time.Time
	lockReason          *int16
	lockRequired        *int32
	lockAvailable       *int32
	lockAvailableFrom   *time.Time
	basisStart          *time.Time
	basisEnd            *time.Time
	basisFullMonths     *int32
	filterFrom          *time.Time
	filterTo            *time.Time
	filterCategoryID    *string
	filterKind          *int16
	filterTransferMode  *int16
	filterSelection     *string
	filterSelectionName *string
	details             []byte
}

func keyOf(s *analyticsv1.PeriodSnapshot) periodKey {
	return periodKey{typ: int16(s.GetPeriod()), key: s.GetPeriodKey(), mode: int16(s.GetTransferMode())}
}

func timeOf(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

func timestampOf(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// errEnumRange is an enum number that does not fit smallint. The number of a value this build
// does not know (a newer producer) is stored as is; one beyond smallint is refused instead of
// being truncated into another value (docs/schema.md, «Инварианты в коде сервиса»).
var errEnumRange = errors.New("enum number out of smallint range")

// errBrokenHeader is a header with removed = false and an empty data column.
var errBrokenHeader = errors.New("snapshot header without its data")

// smallintOf is the column value of an enum number, refusing what smallint cannot hold.
func smallintOf[E ~int32](e E) (int16, error) {
	if e < math.MinInt16 || e > math.MaxInt16 {
		return 0, fmt.Errorf("%w: %d", errEnumRange, e)
	}
	return int16(e), nil
}

// enumColumn maps an optional enum of the contract to its column: UNSPECIFIED is NULL, never 0.
func enumColumn[E ~int32](e E) (*int16, error) {
	if e == 0 {
		return nil, nil
	}
	n, err := smallintOf(e)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func enumOf[E ~int32](n *int16) E {
	if n == nil {
		return 0
	}
	return E(*n)
}

func headerOf(s *analyticsv1.PeriodSnapshot) (headerRow, error) {
	// The same rule as the read (snapshotOf): a snapshot header without its data is refused
	// here, so a producer's error fails the request instead of leaving an unreadable period.
	if s.GetRange().GetStart() == nil || s.GetRange().GetEnd() == nil ||
		s.GetBounds().GetCoverage() == commonv1.Coverage_COVERAGE_UNSPECIFIED || s.GetCalculatedAt() == nil {
		return headerRow{}, fmt.Errorf("%w: snapshot %s without range, coverage or calculated_at", errBrokenHeader, s.GetPeriodKey())
	}
	coverage, err := enumColumn(s.GetBounds().GetCoverage())
	if err != nil {
		return headerRow{}, fmt.Errorf("coverage of %s: %w", s.GetPeriodKey(), err)
	}
	return headerRow{
		periodKey:    keyOf(s),
		rangeStart:   timeOf(s.GetRange().GetStart()),
		rangeEnd:     timeOf(s.GetRange().GetEnd()),
		coverage:     coverage,
		dataFrom:     timeOf(s.GetBounds().GetDataFrom()),
		dataTo:       timeOf(s.GetBounds().GetDataTo()),
		unreadLines:  ptr(s.GetUnreadLinesCount()),
		calculatedAt: timeOf(s.GetCalculatedAt()),
	}, nil
}

// markersOf returns the marker rows of removed_periods: both transfer modes.
func markersOf(r *analyticsv1.PeriodRef) []headerRow {
	key := periodKey{typ: int16(r.GetPeriod()), key: r.GetPeriodKey()}
	with, without := key, key
	with.mode, without.mode = int16(commonv1.TransferMode_TRANSFER_MODE_WITH), int16(commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
	return []headerRow{{periodKey: with, removed: true}, {periodKey: without, removed: true}}
}

func gapsOf(s *analyticsv1.PeriodSnapshot) ([]gapRow, error) {
	if len(s.GetBounds().GetGaps()) > math.MaxInt16+1 {
		return nil, fmt.Errorf("gaps of %s: %d do not fit the smallint ordinal", s.GetPeriodKey(), len(s.GetBounds().GetGaps()))
	}
	gaps := make([]gapRow, 0, len(s.GetBounds().GetGaps()))
	for i, g := range s.GetBounds().GetGaps() {
		gaps = append(gaps, gapRow{periodKey: keyOf(s), ordinal: int16(i), start: timeOf(g.GetStart()), end: timeOf(g.GetEnd())})
	}
	return gaps, nil
}

func metricOf(key periodKey, ms *analyticsv1.MetricSnapshot) (metricRow, error) {
	m := ms.GetMetric()
	fail := func(field string, err error) (metricRow, error) {
		return metricRow{}, fmt.Errorf("%s of %s: %w", field, ms.GetMetricId(), err)
	}
	status, err := smallintOf(m.GetStatus())
	if err != nil {
		return fail("status", err)
	}
	r := metricRow{
		periodKey:         key,
		metricID:          ms.GetMetricId(),
		definitionVersion: ms.GetDefinitionVersion(),
		status:            status,
		valueMinor:        m.ValueMinor,
		rangeStart:        timeOf(m.GetRange().GetStart()),
		rangeEnd:          timeOf(m.GetRange().GetEnd()),
	}
	if l := m.GetLock(); l != nil {
		// The text is not stored: analytics builds it when it assembles the answer.
		reason, err := smallintOf(l.GetReason())
		if err != nil {
			return fail("lock reason", err)
		}
		r.lockReason = &reason
		r.lockRequired, r.lockAvailable = l.Required, l.Available
		r.lockAvailableFrom = timeOf(l.GetAvailableFrom())
	}
	if b := m.GetBasis(); b != nil {
		r.basisStart, r.basisEnd = timeOf(b.GetRange().GetStart()), timeOf(b.GetRange().GetEnd())
		r.basisFullMonths = ptr(b.GetFullMonths())
	}
	if f := m.GetFilters(); f != nil {
		r.filterFrom, r.filterTo = timeOf(f.GetFrom()), timeOf(f.GetTo())
		r.filterCategoryID = f.CategoryId
		if r.filterKind, err = enumColumn(f.GetKind()); err != nil {
			return fail("filter kind", err)
		}
		if r.filterTransferMode, err = enumColumn(f.GetTransferMode()); err != nil {
			return fail("filter transfer mode", err)
		}
		r.filterSelection, r.filterSelectionName = f.Selection, f.SelectionName
	}
	if m.GetDetails() != nil {
		b, err := detailsMarshal.Marshal(&analyticsv1.Metric{Details: m.Details})
		if err != nil {
			return fail("details", err)
		}
		r.details = b
	}
	return r, nil
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

// column turns rows into one array parameter of unnest.
func column[R, T any](rows []R, get func(R) T) []T {
	out := make([]T, len(rows))
	for i, r := range rows {
		out[i] = get(r)
	}
	return out
}

// appliedRow is a key the header statement applied.
type appliedRow struct {
	periodKey
	removed bool
}

// upsertPeriods writes the periods of one request the way the handler is expected to:
// headers and markers in one statement, then, for the applied keys only, the old gaps and
// metrics removed and the new full set inserted. Run it in the request's transaction, after
// the state.
func upsertPeriods(ctx context.Context, q querier, user string, version int64,
	snapshots []*analyticsv1.PeriodSnapshot, removed []*analyticsv1.PeriodRef,
) ([]appliedRow, error) {
	var headers []headerRow
	for _, s := range snapshots {
		h, err := headerOf(s)
		if err != nil {
			return nil, err
		}
		headers = append(headers, h)
	}
	for _, r := range removed {
		headers = append(headers, markersOf(r)...)
	}
	rows, err := q.Query(ctx, upsertHeadersSQL, user, version,
		column(headers, func(h headerRow) int16 { return h.typ }),
		column(headers, func(h headerRow) string { return h.key }),
		column(headers, func(h headerRow) int16 { return h.mode }),
		column(headers, func(h headerRow) bool { return h.removed }),
		column(headers, func(h headerRow) *time.Time { return h.rangeStart }),
		column(headers, func(h headerRow) *time.Time { return h.rangeEnd }),
		column(headers, func(h headerRow) *int16 { return h.coverage }),
		column(headers, func(h headerRow) *time.Time { return h.dataFrom }),
		column(headers, func(h headerRow) *time.Time { return h.dataTo }),
		column(headers, func(h headerRow) *int32 { return h.unreadLines }),
		column(headers, func(h headerRow) *time.Time { return h.calculatedAt }))
	if err != nil {
		return nil, err
	}
	applied, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (appliedRow, error) {
		var a appliedRow
		err := r.Scan(&a.typ, &a.key, &a.mode, &a.removed)
		return a, err
	})
	if err != nil || len(applied) == 0 {
		return applied, err
	}

	if _, err := q.Exec(ctx, deleteChildrenSQL, user,
		column(applied, func(a appliedRow) int16 { return a.typ }),
		column(applied, func(a appliedRow) string { return a.key }),
		column(applied, func(a appliedRow) int16 { return a.mode })); err != nil {
		return nil, fmt.Errorf("delete children: %w", err)
	}

	written := make(map[periodKey]bool, len(applied))
	for _, a := range applied {
		written[a.periodKey] = !a.removed
	}
	var gaps []gapRow
	var metrics []metricRow
	for _, s := range snapshots {
		if !written[keyOf(s)] {
			continue
		}
		g, err := gapsOf(s)
		if err != nil {
			return nil, err
		}
		gaps = append(gaps, g...)
		for _, ms := range s.GetMetrics() {
			r, err := metricOf(keyOf(s), ms)
			if err != nil {
				return nil, err
			}
			metrics = append(metrics, r)
		}
	}
	if len(gaps) > 0 {
		if _, err := q.Exec(ctx, insertGapsSQL, user,
			column(gaps, func(g gapRow) int16 { return g.typ }),
			column(gaps, func(g gapRow) string { return g.key }),
			column(gaps, func(g gapRow) int16 { return g.mode }),
			column(gaps, func(g gapRow) int16 { return g.ordinal }),
			column(gaps, func(g gapRow) *time.Time { return g.start }),
			column(gaps, func(g gapRow) *time.Time { return g.end })); err != nil {
			return nil, fmt.Errorf("insert gaps: %w", err)
		}
	}
	if len(metrics) > 0 {
		if _, err := q.Exec(ctx, insertMetricsSQL, user,
			column(metrics, func(m metricRow) int16 { return m.typ }),
			column(metrics, func(m metricRow) string { return m.key }),
			column(metrics, func(m metricRow) int16 { return m.mode }),
			column(metrics, func(m metricRow) string { return m.metricID }),
			column(metrics, func(m metricRow) int32 { return m.definitionVersion }),
			column(metrics, func(m metricRow) int16 { return m.status }),
			column(metrics, func(m metricRow) *int64 { return m.valueMinor }),
			column(metrics, func(m metricRow) *time.Time { return m.rangeStart }),
			column(metrics, func(m metricRow) *time.Time { return m.rangeEnd }),
			column(metrics, func(m metricRow) *int16 { return m.lockReason }),
			column(metrics, func(m metricRow) *int32 { return m.lockRequired }),
			column(metrics, func(m metricRow) *int32 { return m.lockAvailable }),
			column(metrics, func(m metricRow) *time.Time { return m.lockAvailableFrom }),
			column(metrics, func(m metricRow) *time.Time { return m.basisStart }),
			column(metrics, func(m metricRow) *time.Time { return m.basisEnd }),
			column(metrics, func(m metricRow) *int32 { return m.basisFullMonths }),
			column(metrics, func(m metricRow) *time.Time { return m.filterFrom }),
			column(metrics, func(m metricRow) *time.Time { return m.filterTo }),
			column(metrics, func(m metricRow) *string { return m.filterCategoryID }),
			column(metrics, func(m metricRow) *int16 { return m.filterKind }),
			column(metrics, func(m metricRow) *int16 { return m.filterTransferMode }),
			column(metrics, func(m metricRow) *string { return m.filterSelection }),
			column(metrics, func(m metricRow) *string { return m.filterSelectionName }),
			column(metrics, func(m metricRow) []byte { return m.details })); err != nil {
			return nil, fmt.Errorf("insert metrics: %w", err)
		}
	}
	return applied, nil
}

// analyticsRead is the result of readAnalyticsSQL.
type analyticsRead struct {
	state          state
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
