package postgres

// The repository of UpsertUserMetrics, ListPeriods and PurgeUserData (FIN-283) over the
// schema of FIN-113: the statements and the snapshot ↔ rows mapping of docs/schema.md
// («Запись», «Чтение»). The callers hold the transaction (db.Run) and the tombstone guard.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/analytics/internal/domain"
)

// upsertStateSQL applies UserDataState when its calc_version is not older than the stored one.
// RETURNING gives a row only when the state was written.
const upsertStateSQL = `
INSERT INTO user_data_state AS s
    (user_id, calc_version, has_data, data_from, data_to, last_operation_at, stale_after,
     full_months, recalculating, time_zone, calculated_at,data_revisions)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,CASE WHEN $12::jsonb IS NULL THEN NULL ELSE jsonb_build_object('calc_version',$2::integer,'revisions',$12::jsonb) END)
ON CONFLICT (user_id) DO UPDATE
SET calc_version = EXCLUDED.calc_version,
    has_data = EXCLUDED.has_data,
    data_from = EXCLUDED.data_from,
    data_to = EXCLUDED.data_to,
    last_operation_at = EXCLUDED.last_operation_at,
    stale_after = EXCLUDED.stale_after,
    full_months = EXCLUDED.full_months,
    recalculating = EXCLUDED.recalculating,
    time_zone = EXCLUDED.time_zone,
    calculated_at = EXCLUDED.calculated_at,
    data_revisions=EXCLUDED.data_revisions,
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.user_id`

// upsertHeadersSQL applies the headers of the snapshots and the removal markers of one request
// in one statement. Rows are taken in primary key order, so two requests of the same user lock
// their rows in the same order and cannot deadlock. A key may appear only once per statement
// (ON CONFLICT cannot touch a row twice). RETURNING gives the keys that were applied: only their
// gaps and metrics are replaced.
//
// A snapshot is compared with the stored row of its own key and mode. A removal is compared with
// the greater stored version of both modes of the period, snapshots and markers alike
// (grpc-api.md 7.3, «Версии»): the NOT EXISTS predicate drops the markers of a removal older than
// either row, so a removal is applied to both modes or to none. The predicate reads the statement
// snapshot, so it relies on the caller holding the tombstone guard of the user, which serializes
// every write of that user (docs/schema.md, «Запись»).
const upsertHeadersSQL = `
INSERT INTO period_snapshot AS s
    (user_id, period_type, period_key, transfer_mode, calc_version, removed,
     range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at,data_revisions)
SELECT $1, p.period_type, p.period_key, p.transfer_mode, $2, p.removed,
       p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at,
       CASE WHEN p.data_revisions IS NULL THEN NULL ELSE jsonb_build_object('calc_version',$2::integer,'revisions',p.data_revisions) END
FROM unnest($3::smallint[], $4::text[], $5::smallint[], $6::boolean[],
            $7::timestamptz[], $8::timestamptz[], $9::smallint[], $10::timestamptz[], $11::timestamptz[],
            $12::integer[], $13::timestamptz[],$14::jsonb[])
    AS p (period_type, period_key, transfer_mode, removed,
          range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at,data_revisions)
WHERE NOT p.removed OR NOT EXISTS (
    SELECT 1 FROM period_snapshot o
    WHERE o.user_id = $1 AND o.period_type = p.period_type AND o.period_key = p.period_key
      AND o.calc_version > $2)
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
    data_revisions=EXCLUDED.data_revisions,
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

// listPeriodsSQL is ListPeriods: the zone of the state and the periods of the type with data,
// newest first, one row per period whatever the transfer mode (ListPeriods has none; coverage
// does not depend on it). One statement, so a replica returns the zone and the periods of one
// snapshot of data. The outer VALUES row gives one row with NULL period columns when there is
// neither state nor periods; the lateral subquery reads the primary key in order, the outer
// ORDER BY sorts at most the periods of one user and type (120 months in 10 years).
// The state proof check is materialized once, across every type/mode/removal marker,
// so a header-only old-image write cannot certify a missing/empty selected type.
const listPeriodsSQL = `
WITH proven_state AS MATERIALIZED (
    SELECT s.user_id,s.time_zone,s.calc_version,CASE WHEN NOT EXISTS (
        SELECT 1 FROM period_snapshot newer
        WHERE newer.user_id = s.user_id AND newer.calc_version > s.calc_version
    ) THEN s.data_revisions END AS proven_revisions
    FROM user_data_state s WHERE s.user_id = $1
)
SELECT s.time_zone,p.period_key,p.range_start,p.range_end,p.coverage,s.proven_revisions,p.data_revisions,s.calc_version,p.calc_version
FROM (VALUES ($1::uuid)) AS u (user_id)
LEFT JOIN proven_state s ON s.user_id = u.user_id
LEFT JOIN LATERAL (
    SELECT DISTINCT ON (n.period_key) n.period_key,n.range_start,n.range_end,n.coverage,n.data_revisions,n.calc_version
    FROM period_snapshot n
    WHERE n.user_id = u.user_id AND n.period_type = $2 AND NOT n.removed
    ORDER BY n.period_key DESC, n.transfer_mode DESC
) p ON true
ORDER BY p.period_key DESC`

// purgeSQL is the data part of PurgeUserData, one statement for both tables; gaps and metrics go
// by the cascade of their foreign keys. The tombstone library runs it in its transaction together
// with the tombstone.
const purgeSQL = `
WITH periods AS (DELETE FROM period_snapshot WHERE user_id = $1)
DELETE FROM user_data_state WHERE user_id = $1`

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Metrics is the repository of user metrics. It is safe for concurrent use.
type Metrics struct{ db *DB }

// NewMetrics returns the repository over db.
func NewMetrics(db *DB) *Metrics { return &Metrics{db: db} }

// UpsertState applies the state of one request. Call it inside db.Run, after the tombstone
// guard and before UpsertPeriods: every request writes the state first, so concurrent requests
// of one user lock rows in one order.
func (r *Metrics) UpsertState(ctx context.Context, userID string, version int32, s domain.State) error {
	tx, err := r.transaction(ctx)
	if err != nil {
		return err
	}
	_, err = upsertState(ctx, tx, userID, version, stateRowOf(s))
	return err
}

// ErrTransactionRequired is a write called outside db.Run of this database: the statements
// of one request and the tombstone guard must commit together, never in autocommit.
var ErrTransactionRequired = errors.New("postgres: metrics write outside its transaction")

func (r *Metrics) transaction(ctx context.Context) (pgx.Tx, error) {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return nil, ErrTransactionRequired
	}
	return tx, nil
}

// UpsertPeriods applies the snapshots and removals of one request inside db.Run and returns
// how many snapshots were applied (removals are not counted). Errors are ErrTransactionRequired,
// database errors or errEnumRange for a value the columns cannot hold.
func (r *Metrics) UpsertPeriods(ctx context.Context, userID string, version int32, snapshots []domain.Snapshot, removed []domain.PeriodKey) (int, error) {
	tx, err := r.transaction(ctx)
	if err != nil {
		return 0, err
	}
	applied, err := upsertPeriods(ctx, tx, userID, version, snapshots, removed)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range applied {
		if !a.removed {
			n++
		}
	}
	return n, nil
}

// ListPeriods reads the periods of one type with data, newest first. It reads the replica
// unless the request asked for the primary.
func (r *Metrics) ListPeriods(ctx context.Context, userID string, typ domain.PeriodType) (domain.Periods, error) {
	if typ.Rolling() {
		return r.rollingPeriods(ctx, userID, typ)
	}
	return listPeriods(ctx, r.db.Reader(ctx), userID, typ)
}

func listPeriods(ctx context.Context, q querier, userID string, typ domain.PeriodType) (domain.Periods, error) {
	rows, err := q.Query(ctx, listPeriodsSQL, userID, int16(typ))
	if err != nil {
		return domain.Periods{}, err
	}
	defer rows.Close()
	var out domain.Periods
	var stateRevisions domain.Revisions
	havePeriod := false
	for rows.Next() {
		var (
			zone                        *string
			key                         *string
			start, end                  *time.Time
			coverage                    *int16
			stateJSON, periodJSON       []byte
			stateVersion, periodVersion *int32
		)
		if err := rows.Scan(&zone, &key, &start, &end, &coverage, &stateJSON, &periodJSON, &stateVersion, &periodVersion); err != nil {
			return domain.Periods{}, err
		}
		stateRevisions, err = revisionsFromJSON(stateJSON, stateVersion)
		if err != nil {
			return domain.Periods{}, err
		}
		if zone != nil {
			out.TimeZone = *zone
		}
		if key == nil {
			continue // no periods: the one row of the outer VALUES
		}
		if start == nil || end == nil || coverage == nil {
			return domain.Periods{}, fmt.Errorf("%w: period %s", errBrokenHeader, *key)
		}
		provenance, e := revisionsFromJSON(periodJSON, periodVersion)
		if e != nil {
			return domain.Periods{}, e
		}
		if !havePeriod {
			out.Revisions = provenance
			havePeriod = true
		} else {
			out.Revisions = out.Revisions.Intersect(provenance)
		}
		out.Items = append(out.Items, domain.PeriodItem{Key: *key, Start: *start, End: *end, Coverage: domain.Coverage(*coverage)})
	}
	if err := rows.Err(); err != nil {
		return domain.Periods{}, err
	}
	if !havePeriod {
		out.Revisions = stateRevisions
	} else {
		out.Revisions = out.Revisions.Intersect(stateRevisions)
	}
	return out, nil
}

// Purge deletes every row of the user; gaps and metrics go by cascade. It is the remove
// callback of the tombstone library and runs in its transaction.
func (r *Metrics) Purge(ctx context.Context, userID string) error {
	tx, err := r.transaction(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, purgeSQL, userID)
	return err
}

// stateRow is UserDataState as the columns of user_data_state.
type stateRow struct {
	version              *int32
	revisions            []byte
	hasData              bool
	dataFrom, dataTo     *time.Time
	lastOperation, stale *time.Time
	fullMonths           int32
	recalculating        bool
	timeZone             string
	calculatedAt         *time.Time
}

func stateRowOf(s domain.State) stateRow {
	return stateRow{
		hasData: s.HasData, dataFrom: s.DataFrom, dataTo: s.DataTo,
		lastOperation: s.LastOperationAt, stale: s.StaleAfter, fullMonths: s.FullMonths,
		recalculating: s.Recalculating, timeZone: s.TimeZone, calculatedAt: s.CalculatedAt, revisions: revisionsJSON(s.Revisions),
	}
}

func upsertState(ctx context.Context, q querier, user string, version int32, s stateRow) (applied bool, err error) {
	rows, err := q.Query(ctx, upsertStateSQL, user, version, s.hasData, s.dataFrom, s.dataTo,
		s.lastOperation, s.stale, s.fullMonths, s.recalculating, s.timeZone, s.calculatedAt, s.revisions)
	if err != nil {
		return false, err
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	return n == 1, rows.Err()
}

// periodKey is the key of a snapshot within a user.
type periodKey struct {
	typ  int16
	key  string
	mode int16
}

// headerRow is a row of period_snapshot: a snapshot header or, with removed, a marker.
type headerRow struct {
	version   *int32
	revisions []byte
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

// errEnumRange is an enum number that does not fit smallint. The number of a value this build
// does not know (a newer producer) is stored as is; one beyond smallint is refused instead of
// being truncated into another value (docs/schema.md, «Инварианты в коде сервиса»). The domain
// refuses such a request first; the mapping keeps the check so no path truncates.
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

func ptr[T any](v T) *T { return &v }

func keyOf(k domain.SnapshotKey) periodKey {
	return periodKey{typ: int16(k.Type), key: k.Key, mode: int16(k.Mode)}
}

func rangeBounds(r *domain.TimeRange) (start, end *time.Time) {
	if r == nil {
		return nil, nil
	}
	return r.Start, r.End
}

func headerOf(s domain.Snapshot) (headerRow, error) {
	// The same rule as the read: a snapshot header without its data is refused here, so a
	// producer's error fails the request instead of leaving an unreadable period. A snapshot
	// comes only for a period with covered days (grpc-api.md 7.3), so its data bounds are there
	// too; NULL bounds belong to a removal marker.
	start, end := rangeBounds(s.Range)
	if start == nil || end == nil || s.Coverage == 0 || s.DataFrom == nil || s.DataTo == nil || s.CalculatedAt == nil {
		return headerRow{}, fmt.Errorf("%w: snapshot %s without range, coverage, data bounds or calculated_at", errBrokenHeader, s.Key.Key)
	}
	coverage, err := enumColumn(s.Coverage)
	if err != nil {
		return headerRow{}, fmt.Errorf("coverage of %s: %w", s.Key.Key, err)
	}
	return headerRow{
		periodKey:    keyOf(s.Key),
		rangeStart:   start,
		rangeEnd:     end,
		coverage:     coverage,
		dataFrom:     s.DataFrom,
		dataTo:       s.DataTo,
		unreadLines:  ptr(s.UnreadLines),
		calculatedAt: s.CalculatedAt, revisions: revisionsJSON(s.Revisions),
	}, nil
}

// markersOf returns the marker rows of a removed period: both transfer modes.
func markersOf(r domain.PeriodKey) []headerRow {
	rows := make([]headerRow, 0, len(domain.TransferModes))
	for _, mode := range domain.TransferModes {
		rows = append(rows, headerRow{periodKey: keyOf(domain.SnapshotKey{PeriodKey: r, Mode: mode}), removed: true})
	}
	return rows
}

func gapsOf(s domain.Snapshot) ([]gapRow, error) {
	if len(s.Gaps) > math.MaxInt16+1 {
		return nil, fmt.Errorf("gaps of %s: %d do not fit the smallint ordinal", s.Key.Key, len(s.Gaps))
	}
	gaps := make([]gapRow, 0, len(s.Gaps))
	for i, g := range s.Gaps {
		gaps = append(gaps, gapRow{periodKey: keyOf(s.Key), ordinal: int16(i), start: g.Start, end: g.End})
	}
	return gaps, nil
}

func metricOf(key periodKey, m domain.Metric) (metricRow, error) {
	fail := func(field string, err error) (metricRow, error) {
		return metricRow{}, fmt.Errorf("%s of %s: %w", field, m.ID, err)
	}
	status, err := smallintOf(m.Status)
	if err != nil {
		return fail("status", err)
	}
	start, end := rangeBounds(m.Range)
	r := metricRow{
		periodKey:         key,
		metricID:          m.ID,
		definitionVersion: m.DefinitionVersion,
		status:            status,
		valueMinor:        m.ValueMinor,
		rangeStart:        start,
		rangeEnd:          end,
		details:           m.Details,
	}
	if l := m.Lock; l != nil {
		// The text is not stored: analytics builds it when it assembles the answer.
		reason, err := smallintOf(l.Reason)
		if err != nil {
			return fail("lock reason", err)
		}
		r.lockReason = &reason
		r.lockRequired, r.lockAvailable = l.Required, l.Available
		r.lockAvailableFrom = l.AvailableFrom
	}
	if b := m.Basis; b != nil {
		r.basisStart, r.basisEnd = rangeBounds(b.Range)
		r.basisFullMonths = ptr(b.FullMonths)
	}
	if f := m.Filter; f != nil {
		r.filterFrom, r.filterTo = f.From, f.To
		r.filterCategoryID = f.CategoryID
		if r.filterKind, err = enumColumn(f.Kind); err != nil {
			return fail("filter kind", err)
		}
		if r.filterTransferMode, err = enumColumn(f.TransferMode); err != nil {
			return fail("filter transfer mode", err)
		}
		r.filterSelection, r.filterSelectionName = f.Selection, f.SelectionName
	}
	return r, nil
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

// upsertPeriods writes the periods of one request: headers and markers in one statement, then,
// for the applied keys only, the old gaps and metrics removed and the new full set inserted.
// Run it in the request's transaction, after the state.
func upsertPeriods(ctx context.Context, q querier, user string, version int32,
	snapshots []domain.Snapshot, removed []domain.PeriodKey,
) ([]appliedRow, error) {
	headers := make([]headerRow, 0, len(snapshots)+2*len(removed))
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
	if len(headers) == 0 {
		return nil, nil
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
		column(headers, func(h headerRow) *time.Time { return h.calculatedAt }),
		column(headers, func(h headerRow) []byte { return h.revisions }))
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
		if !written[keyOf(s.Key)] {
			continue
		}
		g, err := gapsOf(s)
		if err != nil {
			return nil, err
		}
		gaps = append(gaps, g...)
		for _, m := range s.Metrics {
			r, err := metricOf(keyOf(s.Key), m)
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
