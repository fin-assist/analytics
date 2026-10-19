package migrations_test

// Integration tests of the analytics schema (FIN-113, docs/schema.md) on a real PostgreSQL:
// the invariants the tables enforce, the versioned upsert of UpsertUserMetrics under
// concurrent delivery, and the read queries the service is expected to run. The SQL below
// is the proposal of docs/schema.md; the repositories will take it from there.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/adapter/postgres/migrate"
	"github.com/fin-assist/analytics/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/analytics/migrations"
)

// upsertStateSQL applies UserDataState when its calc_version is not older than the stored one.
// RETURNING gives a row only when the state was written.
const upsertStateSQL = `
INSERT INTO user_data_state AS s
    (user_id, calc_version, has_data, data_from, data_to, last_operation_at, stale_after,
     full_months, recalculating, time_zone, calculated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.user_id`

// upsertPeriodsSQL applies the snapshots and the removals of one request in one statement.
// A removal is a row with snapshot NULL (a version marker). Rows are taken in primary key
// order, so two requests of the same user lock their rows in the same order and cannot
// deadlock. A key may appear only once per statement (ON CONFLICT cannot touch a row twice).
const upsertPeriodsSQL = `
INSERT INTO period_snapshot AS s
    (user_id, period_type, period_key, transfer_mode, calc_version, range_start, range_end, coverage, snapshot)
SELECT $1, p.period_type, p.period_key, p.transfer_mode, $2, p.range_start, p.range_end, p.coverage, p.snapshot
FROM unnest($3::smallint[], $4::text[], $5::smallint[], $6::timestamptz[], $7::timestamptz[], $8::smallint[], $9::bytea[])
    AS p (period_type, period_key, transfer_mode, range_start, range_end, coverage, snapshot)
ORDER BY p.period_type, p.period_key COLLATE "C", p.transfer_mode
ON CONFLICT (user_id, period_type, period_key, transfer_mode) DO UPDATE
SET calc_version = EXCLUDED.calc_version,
    range_start = EXCLUDED.range_start,
    range_end = EXCLUDED.range_end,
    coverage = EXCLUDED.coverage,
    snapshot = EXCLUDED.snapshot,
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.period_type, s.period_key, s.transfer_mode, s.snapshot IS NULL AS removed`

// readAnalyticsSQL is the one read of GetAnalytics for an exact period key: the state, the
// snapshot of the period in the requested transfer mode and the neighbours with data.
// No row: the user has no state yet. snapshot NULL: no snapshot for this key and mode.
const readAnalyticsSQL = `
SELECT s.has_data, s.data_from, s.data_to, s.last_operation_at, s.stale_after, s.full_months,
       s.recalculating, s.time_zone, s.calculated_at,
       p.snapshot,
       (SELECT max(n.period_key) FROM period_snapshot n
         WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key < $3
           AND n.snapshot IS NOT NULL) AS previous,
       (SELECT min(n.period_key) FROM period_snapshot n
         WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key > $3
           AND n.snapshot IS NOT NULL) AS next
FROM user_data_state s
LEFT JOIN period_snapshot p
       ON p.user_id = s.user_id AND p.period_type = $2 AND p.period_key = $3 AND p.transfer_mode = $4
WHERE s.user_id = $1`

// latestPeriodSQL is the default period of GetAnalytics: the latest period of the type with data.
const latestPeriodSQL = `
SELECT max(period_key) FROM period_snapshot
WHERE user_id = $1 AND period_type = $2 AND snapshot IS NOT NULL`

// listPeriodsSQL is ListPeriods: periods of the type with data, newest first, one row per
// period whatever the transfer mode (ListPeriods has none; coverage does not depend on it).
const listPeriodsSQL = `
SELECT DISTINCT ON (period_key) period_key, range_start, range_end, coverage
FROM period_snapshot
WHERE user_id = $1 AND period_type = $2 AND snapshot IS NOT NULL
ORDER BY period_key DESC, transfer_mode DESC`

// purgeSQL is the data part of PurgeUserData, one statement for both tables; the tombstone
// library runs it in its transaction together with the tombstone.
const purgeSQL = `
WITH periods AS (DELETE FROM period_snapshot WHERE user_id = $1)
DELETE FROM user_data_state WHERE user_id = $1`

const (
	month   int16 = int16(commonv1.PeriodType_PERIOD_TYPE_MONTH)
	quarter int16 = int16(commonv1.PeriodType_PERIOD_TYPE_QUARTER)
	year    int16 = int16(commonv1.PeriodType_PERIOD_TYPE_YEAR)

	withTransfers    int16 = int16(commonv1.TransferMode_TRANSFER_MODE_WITH)
	withoutTransfers int16 = int16(commonv1.TransferMode_TRANSFER_MODE_WITHOUT)

	complete int16 = int16(commonv1.Coverage_COVERAGE_COMPLETE)
	partial  int16 = int16(commonv1.Coverage_COVERAGE_PARTIAL)
)

var msk = time.FixedZone("MSK", 3*3600)

func openDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dsn := migratetest.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Up(ctx, cfg, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newUser(i int) string { return fmt.Sprintf("01a107db-ae80-7b07-b8a1-%012d", i) }

// state is UserDataState as the columns of user_data_state.
type state struct {
	hasData              bool
	dataFrom, dataTo     *time.Time
	lastOperation, stale *time.Time
	fullMonths           int32
	recalculating        bool
	timeZone             string
	calculatedAt         time.Time
}

func ptr[T any](v T) *T { return &v }

func validState() state {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, msk)
	to := time.Date(2026, 9, 26, 0, 0, 0, 0, msk)
	return state{
		hasData: true, dataFrom: &from, dataTo: &to,
		lastOperation: ptr(time.Date(2026, 9, 25, 21, 40, 0, 0, msk)),
		stale:         ptr(time.Date(2026, 10, 10, 0, 0, 0, 0, msk)),
		timeZone:      "Europe/Moscow", calculatedAt: time.Date(2026, 9, 25, 21, 52, 0, 0, msk),
	}
}

func upsertState(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, user string, version int64, s state) (applied bool, err error) {
	rows, err := q.Query(ctx, upsertStateSQL, user, version, s.hasData, s.dataFrom, s.dataTo,
		s.lastOperation, s.stale, s.fullMonths, s.recalculating, s.timeZone, s.calculatedAt)
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

// period is one row of a request: a snapshot or, with snapshot nil, a removal marker.
type period struct {
	typ      int16
	key      string
	mode     int16
	start    *time.Time
	end      *time.Time
	coverage *int16
	snapshot []byte
}

func snapshotRow(typ int16, key string, mode int16, body string) period {
	start, end := monthRange(key)
	return period{typ: typ, key: key, mode: mode, start: &start, end: &end, coverage: ptr(partial), snapshot: []byte(body)}
}

// removal returns the two marker rows of removed_periods: both transfer modes.
func removal(typ int16, key string) []period {
	return []period{{typ: typ, key: key, mode: withTransfers}, {typ: typ, key: key, mode: withoutTransfers}}
}

func monthRange(key string) (time.Time, time.Time) {
	if len(key) == 7 && key[4] == '-' && key[5] != 'Q' {
		start, err := time.ParseInLocation("2006-01", key, msk)
		if err == nil {
			return start, start.AddDate(0, 1, 0)
		}
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, msk)
	return start, start.AddDate(1, 0, 0)
}

type appliedRow struct {
	typ     int16
	key     string
	mode    int16
	removed bool
}

func upsertPeriods(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, user string, version int64, ps []period) ([]appliedRow, error) {
	typs := make([]int16, len(ps))
	keys := make([]string, len(ps))
	modes := make([]int16, len(ps))
	starts := make([]*time.Time, len(ps))
	ends := make([]*time.Time, len(ps))
	coverages := make([]*int16, len(ps))
	bodies := make([][]byte, len(ps))
	for i, p := range ps {
		typs[i], keys[i], modes[i] = p.typ, p.key, p.mode
		starts[i], ends[i], coverages[i], bodies[i] = p.start, p.end, p.coverage, p.snapshot
	}
	rows, err := q.Query(ctx, upsertPeriodsSQL, user, version, typs, keys, modes, starts, ends, coverages, bodies)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (appliedRow, error) {
		var a appliedRow
		err := r.Scan(&a.typ, &a.key, &a.mode, &a.removed)
		return a, err
	})
}

type stored struct {
	version  int64
	snapshot []byte
}

func storedPeriod(t *testing.T, db *pgxpool.Pool, user string, typ int16, key string, mode int16) (stored, bool) {
	t.Helper()
	var s stored
	err := db.QueryRow(context.Background(),
		`SELECT calc_version, snapshot FROM period_snapshot
		 WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
		user, typ, key, mode).Scan(&s.version, &s.snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, true
}

// wantViolation asserts that err is the violation of the named constraint.
func wantViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v, want %s %s", err, code, constraint)
	}
	if pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Fatalf("violation %s %q (%s), want %s %q", pgErr.Code, pgErr.ConstraintName, pgErr.Message, code, constraint)
	}
}

const checkViolation, uniqueViolation = "23514", "23505"

func TestUserDataStateInvariants(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := upsertState(ctx, db, newUser(1), 1, validState()); err != nil {
		t.Fatalf("valid state: %v", err)
	}
	empty := state{timeZone: "Europe/Moscow", calculatedAt: time.Now()}
	if _, err := upsertState(ctx, db, newUser(2), 1, empty); err != nil {
		t.Fatalf("state without covered days: %v", err)
	}
	// Covered days without operations: has_data false, bounds and stale_after set.
	noOps := validState()
	noOps.hasData, noOps.lastOperation = false, nil
	if _, err := upsertState(ctx, db, newUser(3), 1, noOps); err != nil {
		t.Fatalf("covered days without operations: %v", err)
	}

	cases := []struct {
		name       string
		version    int64
		edit       func(*state)
		constraint string
	}{
		{"zero calc_version", 0, func(*state) {}, "user_data_state_calc_version_check"},
		{"data_from without data_to", 1, func(s *state) { s.dataTo, s.stale = nil, nil }, "user_data_state_bounds_check"},
		{"data_to without data_from", 1, func(s *state) { s.dataFrom = nil }, "user_data_state_bounds_check"},
		{"empty bounds", 1, func(s *state) { s.dataTo = s.dataFrom }, "user_data_state_bounds_check"},
		{"stale_after without bounds", 1, func(s *state) { s.hasData, s.dataFrom, s.dataTo = false, nil, nil }, "user_data_state_stale_after_check"},
		{"bounds without stale_after", 1, func(s *state) { s.stale = nil }, "user_data_state_stale_after_check"},
		{"stale_after not after data_to", 1, func(s *state) { s.stale = s.dataTo }, "user_data_state_stale_after_check"},
		{"has_data without operations", 1, func(s *state) { s.lastOperation = nil }, "user_data_state_has_data_check"},
		{"negative full_months", 1, func(s *state) { s.fullMonths = -1 }, "user_data_state_full_months_check"},
		{"empty time zone", 1, func(s *state) { s.timeZone = "" }, "user_data_state_time_zone_check"},
		{"long time zone", 1, func(s *state) { s.timeZone = strings.Repeat("a", 65) }, "user_data_state_time_zone_check"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validState()
			c.edit(&s)
			_, err := upsertState(ctx, db, newUser(100+i), c.version, s)
			wantViolation(t, err, checkViolation, c.constraint)
		})
	}
}

func TestPeriodSnapshotInvariants(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	valid := []period{
		snapshotRow(month, "2026-09", withTransfers, "m"),
		snapshotRow(quarter, "2026-Q3", withoutTransfers, "q"),
		snapshotRow(year, "2026", withTransfers, "y"),
	}
	valid = append(valid, removal(month, "2026-08")...)
	if _, err := upsertPeriods(ctx, db, user, 1, valid); err != nil {
		t.Fatalf("valid rows: %v", err)
	}

	insert := func(p period, version int64) error {
		_, err := db.Exec(ctx, `INSERT INTO period_snapshot
			(user_id, period_type, period_key, transfer_mode, calc_version, range_start, range_end, coverage, snapshot)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			user, p.typ, p.key, p.mode, version, p.start, p.end, p.coverage, p.snapshot)
		return err
	}
	edit := func(p period, f func(*period)) period { f(&p); return p }
	base := snapshotRow(month, "2026-10", withTransfers, "x")

	cases := []struct {
		name       string
		row        period
		version    int64
		code       string
		constraint string
	}{
		{"zero calc_version", base, 0, checkViolation, "period_snapshot_calc_version_check"},
		{"unspecified period type", edit(base, func(p *period) { p.typ = 0 }), 1, checkViolation, "period_snapshot_period_type_check"},
		{"unknown period type", edit(base, func(p *period) { p.typ = 4 }), 1, checkViolation, "period_snapshot_period_type_check"},
		{"unspecified transfer mode", edit(base, func(p *period) { p.mode = 0 }), 1, checkViolation, "period_snapshot_transfer_mode_check"},
		{"month 13", edit(base, func(p *period) { p.key = "2026-13" }), 1, checkViolation, "period_snapshot_period_key_check"},
		{"quarter key for a month", edit(base, func(p *period) { p.key = "2026-Q3" }), 1, checkViolation, "period_snapshot_period_key_check"},
		{"quarter 5", edit(base, func(p *period) { p.typ, p.key = quarter, "2026-Q5" }), 1, checkViolation, "period_snapshot_period_key_check"},
		{"month key for a year", edit(base, func(p *period) { p.typ, p.key = year, "2026-09" }), 1, checkViolation, "period_snapshot_period_key_check"},
		{"key with spaces", edit(base, func(p *period) { p.key = " 2026-10" }), 1, checkViolation, "period_snapshot_period_key_check"},
		{"snapshot without range", edit(base, func(p *period) { p.start, p.end = nil, nil }), 1, checkViolation, "period_snapshot_removed_check"},
		{"snapshot without coverage", edit(base, func(p *period) { p.coverage = nil }), 1, checkViolation, "period_snapshot_removed_check"},
		{"marker with range", edit(base, func(p *period) { p.snapshot = nil }), 1, checkViolation, "period_snapshot_removed_check"},
		{"empty range", edit(base, func(p *period) { p.end = p.start }), 1, checkViolation, "period_snapshot_range_check"},
		{"coverage no_data", edit(base, func(p *period) { p.coverage = ptr(int16(commonv1.Coverage_COVERAGE_NO_DATA)) }), 1, checkViolation, "period_snapshot_coverage_check"},
		{"second row of a key", snapshotRow(month, "2026-09", withTransfers, "dup"), 1, uniqueViolation, "period_snapshot_pkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantViolation(t, insert(c.row, c.version), c.code, c.constraint)
		})
	}

	// A key twice in one request (a period both sent and removed) fails the whole statement:
	// the handler validates the request before writing.
	_, err := upsertPeriods(ctx, db, user, 2, append([]period{snapshotRow(month, "2026-07", withTransfers, "a")}, removal(month, "2026-07")...))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "21000" {
		t.Fatalf("duplicate key in one request: %v, want 21000 cardinality_violation", err)
	}
	if _, ok := storedPeriod(t, db, user, month, "2026-07", withTransfers); ok {
		t.Fatal("the failed statement wrote a row")
	}
}

func TestStateAppliesByVersion(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	v5 := validState()
	v5.fullMonths = 5
	if ok, err := upsertState(ctx, db, user, 5, v5); err != nil || !ok {
		t.Fatalf("first state: applied %v, %v", ok, err)
	}
	older := validState()
	older.fullMonths, older.recalculating = 4, true
	if ok, err := upsertState(ctx, db, user, 4, older); err != nil || ok {
		t.Fatalf("older state: applied %v, %v; want skipped", ok, err)
	}
	same := validState()
	same.fullMonths = 5
	if ok, err := upsertState(ctx, db, user, 5, same); err != nil || !ok {
		t.Fatalf("redelivery of the same version: applied %v, %v; want applied", ok, err)
	}
	newer := validState()
	newer.fullMonths, newer.recalculating = 6, true
	if ok, err := upsertState(ctx, db, user, 6, newer); err != nil || !ok {
		t.Fatalf("newer state: applied %v, %v", ok, err)
	}
	var version int64
	var full int32
	var recalculating bool
	if err := db.QueryRow(ctx, `SELECT calc_version, full_months, recalculating FROM user_data_state WHERE user_id = $1`, user).
		Scan(&version, &full, &recalculating); err != nil {
		t.Fatal(err)
	}
	if version != 6 || full != 6 || !recalculating {
		t.Fatalf("stored v%d full_months=%d recalculating=%v, want v6 6 true", version, full, recalculating)
	}
}

func TestPeriodsApplyByVersionPerKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	applied, err := upsertPeriods(ctx, db, user, 5, []period{
		snapshotRow(month, "2026-09", withTransfers, "sep v5"),
		snapshotRow(month, "2026-08", withTransfers, "aug v5"),
	})
	if err != nil || len(applied) != 2 {
		t.Fatalf("first snapshots: %v %v", applied, err)
	}

	// A request of an older version is skipped for the key it is older on, and applied for
	// a key it is the first for: the version is per key, not per user.
	applied, err = upsertPeriods(ctx, db, user, 4, []period{
		snapshotRow(month, "2026-09", withTransfers, "sep v4"),
		snapshotRow(month, "2026-07", withTransfers, "jul v4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].key != "2026-07" {
		t.Fatalf("applied %v, want only 2026-07 (skipped_stale_periods = 1)", applied)
	}
	if s, _ := storedPeriod(t, db, user, month, "2026-09", withTransfers); s.version != 5 || string(s.snapshot) != "sep v5" {
		t.Fatalf("2026-09 is v%d %q after an older delivery, want v5", s.version, s.snapshot)
	}

	// The same version again (outbox redelivery) is applied and changes nothing.
	applied, err = upsertPeriods(ctx, db, user, 5, []period{snapshotRow(month, "2026-09", withTransfers, "sep v5")})
	if err != nil || len(applied) != 1 {
		t.Fatalf("redelivery: %v %v", applied, err)
	}

	// The two transfer modes of one period are separate keys.
	if _, err := upsertPeriods(ctx, db, user, 6, []period{snapshotRow(month, "2026-09", withoutTransfers, "sep without v6")}); err != nil {
		t.Fatal(err)
	}
	if s, _ := storedPeriod(t, db, user, month, "2026-09", withTransfers); s.version != 5 {
		t.Fatalf("mode with is v%d after a snapshot of mode without, want v5", s.version)
	}
}

func TestRemovedPeriodStaysRemovedForOlderSnapshots(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	if _, err := upsertPeriods(ctx, db, user, 3, []period{
		snapshotRow(month, "2026-08", withTransfers, "aug v3"),
		snapshotRow(month, "2026-08", withoutTransfers, "aug without v3"),
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := upsertPeriods(ctx, db, user, 5, removal(month, "2026-08"))
	if err != nil || len(applied) != 2 || !applied[0].removed || !applied[1].removed {
		t.Fatalf("removal: %v %v", applied, err)
	}
	// A snapshot of version 4, delivered after the removal of version 5, must not bring the
	// period back: the marker keeps version 5.
	applied, err = upsertPeriods(ctx, db, user, 4, []period{snapshotRow(month, "2026-08", withTransfers, "aug v4")})
	if err != nil || len(applied) != 0 {
		t.Fatalf("late snapshot after removal: applied %v, %v; want skipped", applied, err)
	}
	if s, ok := storedPeriod(t, db, user, month, "2026-08", withTransfers); !ok || s.snapshot != nil || s.version != 5 {
		t.Fatalf("2026-08 is %+v (present %v), want a v5 removal marker", s, ok)
	}
	var latest *string
	if err := db.QueryRow(ctx, latestPeriodSQL, user, month).Scan(&latest); err != nil || latest != nil {
		t.Fatalf("latest period %v, %v; want none: markers are not periods with data", latest, err)
	}

	// A removal of an older version does not remove a newer snapshot.
	if _, err := upsertPeriods(ctx, db, user, 7, []period{snapshotRow(month, "2026-08", withTransfers, "aug v7")}); err != nil {
		t.Fatal(err)
	}
	applied, err = upsertPeriods(ctx, db, user, 6, removal(month, "2026-08"))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].mode != withoutTransfers {
		t.Fatalf("older removal applied to %v, want only the mode without (still on v5)", applied)
	}
	if s, _ := storedPeriod(t, db, user, month, "2026-08", withTransfers); string(s.snapshot) != "aug v7" {
		t.Fatalf("2026-08 is %q after an older removal, want the v7 snapshot", s.snapshot)
	}
	// A removal of a period the service never saw leaves markers too.
	if applied, err := upsertPeriods(ctx, db, user, 8, removal(month, "2025-01")); err != nil || len(applied) != 2 {
		t.Fatalf("removal of an unknown period: %v %v", applied, err)
	}
}

// One request: the state first, then all period rows in one statement, in one transaction —
// the order the handler is expected to use.
func deliver(ctx context.Context, db *pgxpool.Pool, user string, version int64, withState bool, ps []period) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if withState {
		s := validState()
		s.fullMonths = int32(version)
		if _, err := upsertState(ctx, tx, user, version, s); err != nil {
			return fmt.Errorf("state v%d: %w", version, err)
		}
	}
	if _, err := upsertPeriods(ctx, tx, user, version, ps); err != nil {
		return fmt.Errorf("periods v%d: %w", version, err)
	}
	return tx.Commit(ctx)
}

// Concurrent and out-of-order delivery: several requests of one user race on the same keys,
// in shuffled order, half of them without the state (so they do not serialise on its row),
// some removing a period the others send. Whatever the interleaving: no deadlock, no error,
// and every key ends at the highest version with that version's content.
func TestConcurrentDeliveryKeepsHighestVersion(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	keys := []string{"2026-05", "2026-06", "2026-07", "2026-08", "2026-09"}
	const writers = 8

	for round := range 15 {
		user := newUser(1000 + round)
		// What request v did: removed 2026-07 when v is divisible by 3, otherwise sent it.
		removes := func(v int64) bool { return v%3 == 0 }
		start := make(chan struct{})
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for _, v := range rand.Perm(writers) {
			version := int64(v + 1)
			var ps []period
			for _, k := range keys {
				if k == "2026-07" && removes(version) {
					ps = append(ps, removal(month, k)...)
					continue
				}
				body := fmt.Sprintf("%s v%d", k, version)
				ps = append(ps, snapshotRow(month, k, withTransfers, body), snapshotRow(month, k, withoutTransfers, body))
			}
			rand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- deliver(ctx, db, user, version, version%2 == 0, ps)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}

		for _, k := range keys {
			for _, mode := range []int16{withTransfers, withoutTransfers} {
				s, ok := storedPeriod(t, db, user, month, k, mode)
				if !ok || s.version != writers {
					t.Fatalf("round %d: %s mode %d is v%d (present %v), want v%d", round, k, mode, s.version, ok, writers)
				}
				want := fmt.Sprintf("%s v%d", k, writers)
				if k == "2026-07" && removes(writers) {
					want = ""
				}
				if string(s.snapshot) != want {
					t.Fatalf("round %d: %s mode %d holds %q, want %q", round, k, mode, s.snapshot, want)
				}
			}
		}
		var version int64
		if err := db.QueryRow(ctx, `SELECT calc_version FROM user_data_state WHERE user_id = $1`, user).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != writers {
			t.Fatalf("round %d: state is v%d, want v%d", round, version, writers)
		}
	}
}

// The column holds the PeriodSnapshot message as received: it comes back equal, including a
// metric_id the composition does not know yet and the definition_version of every metric.
func TestSnapshotRoundTripsAsProtobuf(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	msg := &analyticsv1.PeriodSnapshot{
		Period:           commonv1.PeriodType_PERIOD_TYPE_MONTH,
		PeriodKey:        "2026-09",
		TransferMode:     commonv1.TransferMode_TRANSFER_MODE_WITH,
		UnreadLinesCount: 5,
		Metrics: []*analyticsv1.MetricSnapshot{
			{MetricId: "tiles.expense", DefinitionVersion: 1, Metric: &analyticsv1.Metric{
				Status: analyticsv1.MetricStatus_METRIC_STATUS_READY, ValueMinor: proto.Int64(8432000)}},
			{MetricId: "insights.future_metric", DefinitionVersion: 3, Metric: &analyticsv1.Metric{
				Status: analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE}},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	p := snapshotRow(month, "2026-09", withTransfers, "")
	p.snapshot = body
	if _, err := upsertPeriods(ctx, db, user, 1, []period{p}); err != nil {
		t.Fatal(err)
	}
	s, _ := storedPeriod(t, db, user, month, "2026-09", withTransfers)
	got := &analyticsv1.PeriodSnapshot{}
	if err := proto.Unmarshal(s.snapshot, got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, msg) {
		t.Fatalf("snapshot came back as %v, want %v", got, msg)
	}
}

func seedForReads(t *testing.T, db *pgxpool.Pool, user string) {
	t.Helper()
	ctx := context.Background()
	if _, err := upsertState(ctx, db, user, 10, validState()); err != nil {
		t.Fatal(err)
	}
	var ps []period
	for _, k := range []string{"2026-05", "2026-06", "2026-08", "2026-09"} {
		ps = append(ps, snapshotRow(month, k, withTransfers, k+" with"), snapshotRow(month, k, withoutTransfers, k+" without"))
	}
	ps = append(ps, removal(month, "2026-07")...)
	ps = append(ps, snapshotRow(quarter, "2026-Q2", withTransfers, "q2"), snapshotRow(year, "2026", withTransfers, "y"))
	if _, err := upsertPeriods(ctx, db, user, 10, ps); err != nil {
		t.Fatal(err)
	}
	// 2026-06 is complete in both modes.
	if _, err := db.Exec(ctx, `UPDATE period_snapshot SET coverage = $3 WHERE user_id = $1 AND period_key = $2`, user, "2026-06", complete); err != nil {
		t.Fatal(err)
	}
}

func TestReadQueries(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user, other := newUser(1), newUser(2)
	seedForReads(t, db, user)
	seedForReads(t, db, other)
	if _, err := upsertPeriods(ctx, db, other, 11, []period{snapshotRow(month, "2026-10", withTransfers, "other")}); err != nil {
		t.Fatal(err)
	}

	t.Run("latest period", func(t *testing.T) {
		var latest string
		if err := db.QueryRow(ctx, latestPeriodSQL, user, month).Scan(&latest); err != nil || latest != "2026-09" {
			t.Fatalf("latest %q, %v; want 2026-09 (2026-10 belongs to another user)", latest, err)
		}
	})

	t.Run("analytics for a period with navigation over a removed one", func(t *testing.T) {
		var (
			hasData, recalculating bool
			dataFrom, dataTo       *time.Time
			lastOp, staleAfter     *time.Time
			fullMonths             int32
			zone                   string
			calculatedAt           time.Time
			snapshot               []byte
			previous, next         *string
		)
		err := db.QueryRow(ctx, readAnalyticsSQL, user, month, "2026-08", withoutTransfers).Scan(
			&hasData, &dataFrom, &dataTo, &lastOp, &staleAfter, &fullMonths, &recalculating, &zone, &calculatedAt,
			&snapshot, &previous, &next)
		if err != nil {
			t.Fatal(err)
		}
		if !hasData || zone != "Europe/Moscow" || string(snapshot) != "2026-08 without" {
			t.Fatalf("has_data %v zone %q snapshot %q", hasData, zone, snapshot)
		}
		if previous == nil || *previous != "2026-06" || next == nil || *next != "2026-09" {
			t.Fatalf("navigation %v / %v, want 2026-06 (2026-07 removed) / 2026-09", deref(previous), deref(next))
		}
	})

	t.Run("analytics for a removed period and an unknown user", func(t *testing.T) {
		var snapshot []byte
		var previous, next *string
		err := db.QueryRow(ctx, `SELECT q.snapshot, q.previous, q.next FROM (`+readAnalyticsSQL+`) q`, user, month, "2026-07", withTransfers).
			Scan(&snapshot, &previous, &next)
		if err != nil || snapshot != nil || deref(previous) != "2026-06" || deref(next) != "2026-08" {
			t.Fatalf("removed period: snapshot %q navigation %s / %s, %v", snapshot, deref(previous), deref(next), err)
		}
		err = db.QueryRow(ctx, readAnalyticsSQL, newUser(99), month, "2026-09", withTransfers).Scan()
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unknown user: %v, want no rows (has_data = false)", err)
		}
	})

	t.Run("first and last period have no neighbour", func(t *testing.T) {
		var previous, next *string
		q := `SELECT q.previous, q.next FROM (` + readAnalyticsSQL + `) q`
		if err := db.QueryRow(ctx, q, user, month, "2026-05", withTransfers).Scan(&previous, &next); err != nil || previous != nil {
			t.Fatalf("2026-05 previous %s, %v; want none", deref(previous), err)
		}
		if err := db.QueryRow(ctx, q, user, month, "2026-09", withTransfers).Scan(&previous, &next); err != nil || next != nil {
			t.Fatalf("2026-09 next %s, %v; want none (2026-10 belongs to another user)", deref(next), err)
		}
	})

	t.Run("list periods", func(t *testing.T) {
		rows, err := db.Query(ctx, listPeriodsSQL, user, month)
		if err != nil {
			t.Fatal(err)
		}
		type item struct {
			key      string
			coverage int16
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
			var i item
			var s, e time.Time
			return i, r.Scan(&i.key, &s, &e, &i.coverage)
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []item{{"2026-09", partial}, {"2026-08", partial}, {"2026-06", complete}, {"2026-05", partial}}
		if !slices.Equal(got, want) {
			t.Fatalf("periods %v, want %v: newest first, one per period, no removed ones", got, want)
		}
	})

	t.Run("purge removes only this user", func(t *testing.T) {
		if _, err := db.Exec(ctx, purgeSQL, user); err != nil {
			t.Fatal(err)
		}
		var mine, theirs int
		if err := db.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM period_snapshot WHERE user_id = $1) + (SELECT count(*) FROM user_data_state WHERE user_id = $1),
			(SELECT count(*) FROM period_snapshot WHERE user_id = $2) + (SELECT count(*) FROM user_data_state WHERE user_id = $2)`,
			user, other).Scan(&mine, &theirs); err != nil {
			t.Fatal(err)
		}
		if mine != 0 || theirs == 0 {
			t.Fatalf("after purge: %d rows of the user, %d of another; want 0 and all", mine, theirs)
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

// Every expected query is served by the primary key, without a sort, a scan of other users'
// rows or another index. The planner is pushed off sequential scans because the test tables
// are tiny; what is checked is that an index path exists and needs no sort.
func TestQueriesUsePrimaryKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	for i := range 3 {
		seedForReads(t, db, newUser(i))
	}
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	user := newUser(1)
	cases := []struct {
		name  string
		sql   string
		args  []any
		index []string
	}{
		{"get analytics", readAnalyticsSQL, []any{user, month, "2026-08", withTransfers}, []string{"user_data_state_pkey", "period_snapshot_pkey"}},
		{"latest period", latestPeriodSQL, []any{user, month}, []string{"period_snapshot_pkey"}},
		{"list periods", listPeriodsSQL, []any{user, month}, []string{"period_snapshot_pkey"}},
		{"purge snapshots", `DELETE FROM period_snapshot WHERE user_id = $1`, []any{user}, []string{"period_snapshot_pkey"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := conn.Query(ctx, "EXPLAIN "+c.sql, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(lines, "\n")
			for _, idx := range c.index {
				if !strings.Contains(plan, idx) {
					t.Errorf("plan does not use %s:\n%s", idx, plan)
				}
			}
			for _, bad := range []string{"Seq Scan", "Sort"} {
				if strings.Contains(plan, bad) {
					t.Errorf("plan has %s:\n%s", bad, plan)
				}
			}
		})
	}
}
