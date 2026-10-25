package postgres

// Integration tests of the analytics schema (FIN-113, docs/schema.md) on a real PostgreSQL:
// the keys and cascades the tables enforce, the versioned upsert of UpsertUserMetrics with the
// full set of metrics under concurrent delivery, the round trip of PeriodSnapshot through the
// normalised rows, and the read queries. The write, ListPeriods and purge SQL and the mapping are
// the repository (metrics.go); the GetAnalytics read is the FIN-5 proposal (metrics_read_test.go).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"regexp"
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
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-migrate"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/testdb"
	"github.com/fin-assist/analytics/migrations"
)

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
	dsn := testdb.DSN(t)
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

// validState is a UserDataState of a user with data, as the columns of user_data_state.
func validState() stateRow {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, msk)
	to := time.Date(2026, 9, 26, 0, 0, 0, 0, msk)
	return stateRow{
		hasData: true, dataFrom: &from, dataTo: &to,
		lastOperation: ptr(time.Date(2026, 9, 25, 21, 40, 0, 0, msk)),
		stale:         ptr(time.Date(2026, 10, 10, 0, 0, 0, 0, msk)),
		timeZone:      "Europe/Moscow", calculatedAt: ptr(time.Date(2026, 9, 25, 21, 52, 0, 0, msk)),
	}
}

// upsertProto writes protobuf snapshots through the conversion of the handler and the
// production mapping, without the validation of the usecase: the schema tests also send what
// the validation would refuse, to check what the tables themselves keep.
func upsertProto(ctx context.Context, q querier, user string, version int32,
	snapshots []*analyticsv1.PeriodSnapshot, removed []*analyticsv1.PeriodRef,
) ([]appliedRow, error) {
	var ss []domain.Snapshot
	for _, p := range snapshots {
		s, err := metricsgrpc.SnapshotOf(p)
		if err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	var rs []domain.PeriodKey
	for _, r := range removed {
		rs = append(rs, domain.PeriodKey{Type: domain.PeriodType(r.GetPeriod()), Key: r.GetPeriodKey()})
	}
	return upsertPeriods(ctx, q, user, version, ss, rs)
}

// protoKey is the key of a protobuf snapshot.
func protoKey(s *analyticsv1.PeriodSnapshot) periodKey {
	return periodKey{typ: int16(s.GetPeriod()), key: s.GetPeriodKey(), mode: int16(s.GetTransferMode())}
}

func at(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func periodRange(typ int16, key string) (time.Time, time.Time) {
	switch typ {
	case month:
		start, err := time.ParseInLocation("2006-01", key, msk)
		if err != nil {
			panic(err)
		}
		return start, start.AddDate(0, 1, 0)
	case quarter:
		y, err := time.ParseInLocation("2006", key[:4], msk)
		if err != nil {
			panic(err)
		}
		start := y.AddDate(0, 3*int(key[6]-'1'), 0)
		return start, start.AddDate(0, 3, 0)
	default:
		start, err := time.ParseInLocation("2006", key, msk)
		if err != nil {
			panic(err)
		}
		return start, start.AddDate(1, 0, 0)
	}
}

// snapshot builds a PeriodSnapshot whose content identifies the version that sent it:
// unread_lines_count and the value of every metric are the version.
func snapshot(typ int16, key string, mode int16, version int32, metricIDs ...string) *analyticsv1.PeriodSnapshot {
	start, end := periodRange(typ, key)
	s := &analyticsv1.PeriodSnapshot{
		Period:           commonv1.PeriodType(typ),
		PeriodKey:        key,
		TransferMode:     commonv1.TransferMode(mode),
		Range:            &commonv1.TimeRange{Start: at(start), End: at(end)},
		Bounds:           &commonv1.DataBounds{Coverage: commonv1.Coverage(partial), DataFrom: at(start), DataTo: at(start.Add(18 * time.Hour))},
		UnreadLinesCount: version,
		CalculatedAt:     at(time.Date(2026, 9, 25, 21, 52, 0, 0, msk)),
	}
	for _, id := range metricIDs {
		s.Metrics = append(s.Metrics, &analyticsv1.MetricSnapshot{MetricId: id, DefinitionVersion: 1, Metric: &analyticsv1.Metric{
			Status: analyticsv1.MetricStatus_METRIC_STATUS_READY, ValueMinor: proto.Int64(int64(version)),
		}})
	}
	return s
}

// withGaps adds n one-day gaps from the start of the period.
func withGaps(s *analyticsv1.PeriodSnapshot, n int) *analyticsv1.PeriodSnapshot {
	start := s.GetRange().GetStart().AsTime()
	for i := range n {
		day := start.AddDate(0, 0, 2*i+1)
		s.Bounds.Gaps = append(s.Bounds.Gaps, &commonv1.TimeRange{Start: at(day), End: at(day.AddDate(0, 0, 1))})
	}
	return s
}

// both returns the snapshots of a period in both transfer modes.
func both(typ int16, key string, version int32, metricIDs ...string) []*analyticsv1.PeriodSnapshot {
	return []*analyticsv1.PeriodSnapshot{
		snapshot(typ, key, withTransfers, version, metricIDs...),
		snapshot(typ, key, withoutTransfers, version, metricIDs...),
	}
}

func ref(typ int16, key string) *analyticsv1.PeriodRef {
	return &analyticsv1.PeriodRef{Period: commonv1.PeriodType(typ), PeriodKey: key}
}

func pk(typ int16, key string, mode int16) periodKey {
	return periodKey{typ: typ, key: key, mode: mode}
}

// stored is what the tables hold for one key: the header and, by metric_id, the values.
type stored struct {
	version  int32
	removed  bool
	unread   *int32
	metrics  map[string]int64
	gaps     int
	coverage *int16
}

func storedPeriod(t *testing.T, db *pgxpool.Pool, user string, k periodKey) (stored, bool) {
	t.Helper()
	ctx := context.Background()
	var s stored
	err := db.QueryRow(ctx,
		`SELECT calc_version, removed, unread_lines_count, coverage FROM period_snapshot
		 WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
		user, k.typ, k.key, k.mode).Scan(&s.version, &s.removed, &s.unread, &s.coverage)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false
	}
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx,
		`SELECT metric_id, value_minor FROM period_metric
		 WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
		user, k.typ, k.key, k.mode)
	if err != nil {
		t.Fatal(err)
	}
	s.metrics = map[string]int64{}
	for rows.Next() {
		var id string
		var v int64
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		s.metrics[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM period_gap
		 WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
		user, k.typ, k.key, k.mode).Scan(&s.gaps); err != nil {
		t.Fatal(err)
	}
	return s, true
}

// wantMetrics asserts the metric set of a key: every ID with the value of version.
func wantMetrics(t *testing.T, s stored, version int32, ids ...string) {
	t.Helper()
	want := map[string]int64{}
	for _, id := range ids {
		want[id] = int64(version)
	}
	if !maps.Equal(s.metrics, want) {
		t.Fatalf("metrics %v, want %v", s.metrics, want)
	}
}

// wantViolation asserts that err is the violation of the named constraint (or of NOT NULL on
// the named column).
func wantViolation(t *testing.T, err error, code, name string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v, want %s %s", err, code, name)
	}
	got := pgErr.ConstraintName
	if code == notNullViolation {
		got = pgErr.ColumnName
	}
	if pgErr.Code != code || got != name {
		t.Fatalf("violation %s %q (%s), want %s %q", pgErr.Code, got, pgErr.Message, code, name)
	}
}

const notNullViolation, uniqueViolation, foreignKeyViolation = "23502", "23505", "23503"

// The schema keeps only keys and NOT NULL (docs/schema.md, «Инварианты в коде сервиса»):
// every shape of a valid state is accepted, required columns are required.
func TestIntegrationUserDataStateConstraints(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := upsertState(ctx, db, newUser(1), 1, validState()); err != nil {
		t.Fatalf("valid state: %v", err)
	}
	empty := stateRow{timeZone: "Europe/Moscow", calculatedAt: ptr(time.Now())}
	if _, err := upsertState(ctx, db, newUser(2), 1, empty); err != nil {
		t.Fatalf("state without covered days: %v", err)
	}
	// Covered days without operations: has_data false, bounds and stale_after set.
	noOps := validState()
	noOps.hasData, noOps.lastOperation = false, nil
	if _, err := upsertState(ctx, db, newUser(3), 1, noOps); err != nil {
		t.Fatalf("covered days without operations: %v", err)
	}

	insert := func(column string, value any) error {
		args := map[string]any{
			"user_id": newUser(10), "calc_version": int32(1), "has_data": false, "full_months": int32(0),
			"recalculating": false, "time_zone": "Europe/Moscow", "calculated_at": time.Now(),
		}
		args[column] = value
		_, err := db.Exec(ctx, `INSERT INTO user_data_state
			(user_id, calc_version, has_data, full_months, recalculating, time_zone, calculated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			args["user_id"], args["calc_version"], args["has_data"], args["full_months"],
			args["recalculating"], args["time_zone"], args["calculated_at"])
		return err
	}
	for _, column := range []string{"user_id", "calc_version", "has_data", "full_months", "recalculating", "time_zone", "calculated_at"} {
		t.Run(column+" is required", func(t *testing.T) {
			wantViolation(t, insert(column, nil), notNullViolation, column)
		})
	}

	_, err := db.Exec(ctx, `INSERT INTO user_data_state
		(user_id, calc_version, has_data, full_months, recalculating, time_zone, calculated_at)
		VALUES ($1, 1, false, 0, false, 'UTC', now())`, newUser(1))
	wantViolation(t, err, uniqueViolation, "user_data_state_pkey")

	var applied time.Time
	if err := db.QueryRow(ctx, `SELECT applied_at FROM user_data_state WHERE user_id = $1`, newUser(1)).Scan(&applied); err != nil || applied.IsZero() {
		t.Fatalf("applied_at %v, %v; want the default now()", applied, err)
	}
}

// The period tables keep keys, NOT NULL and the cascades of their foreign keys; every other rule
// is the code's (docs/schema.md, «Инварианты в коде сервиса»).
func TestIntegrationPeriodTablesConstraints(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)

	valid := []*analyticsv1.PeriodSnapshot{
		withGaps(snapshot(month, "2026-09", withTransfers, 1, "tiles.expense", "tiles.income"), 2),
		snapshot(quarter, "2026-Q3", withoutTransfers, 1),
		snapshot(year, "2026", withTransfers, 1, "tiles.expense"),
	}
	if _, err := upsertProto(ctx, db, user, 1, valid, []*analyticsv1.PeriodRef{ref(month, "2026-08")}); err != nil {
		t.Fatalf("valid rows: %v", err)
	}

	t.Run("header", func(t *testing.T) {
		insert := func(column string) error {
			args := map[string]any{
				"user_id": user, "period_type": month, "period_key": "2026-10", "transfer_mode": withTransfers,
				"calc_version": int32(1), "removed": true,
			}
			args[column] = nil
			_, err := db.Exec(ctx, `INSERT INTO period_snapshot
				(user_id, period_type, period_key, transfer_mode, calc_version, removed) VALUES ($1, $2, $3, $4, $5, $6)`,
				args["user_id"], args["period_type"], args["period_key"], args["transfer_mode"], args["calc_version"], args["removed"])
			return err
		}
		for _, column := range []string{"user_id", "period_type", "period_key", "transfer_mode", "calc_version", "removed"} {
			t.Run(column+" is required", func(t *testing.T) {
				wantViolation(t, insert(column), notNullViolation, column)
			})
		}
		_, err := db.Exec(ctx, `INSERT INTO period_snapshot
			(user_id, period_type, period_key, transfer_mode, calc_version, removed) VALUES ($1, $2, $3, $4, 1, false)`,
			user, month, "2026-09", withTransfers)
		wantViolation(t, err, uniqueViolation, "period_snapshot_pkey")
	})

	t.Run("gap", func(t *testing.T) {
		insert := func(column string, key string) error {
			args := map[string]any{
				"user_id": user, "period_type": month, "period_key": key, "transfer_mode": withTransfers,
				"ordinal": int16(5), "gap_start": time.Now(), "gap_end": time.Now(),
			}
			if column != "" {
				args[column] = nil
			}
			_, err := db.Exec(ctx, `INSERT INTO period_gap
				(user_id, period_type, period_key, transfer_mode, ordinal, gap_start, gap_end) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				args["user_id"], args["period_type"], args["period_key"], args["transfer_mode"], args["ordinal"],
				args["gap_start"], args["gap_end"])
			return err
		}
		for _, column := range []string{"ordinal", "gap_start", "gap_end"} {
			t.Run(column+" is required", func(t *testing.T) {
				wantViolation(t, insert(column, "2026-09"), notNullViolation, column)
			})
		}
		wantViolation(t, insert("", "2026-07"), foreignKeyViolation, "period_gap_period_snapshot_fkey")
		_, err := db.Exec(ctx, `INSERT INTO period_gap
			(user_id, period_type, period_key, transfer_mode, ordinal, gap_start, gap_end) VALUES ($1, $2, $3, $4, 0, now(), now())`,
			user, month, "2026-09", withTransfers)
		wantViolation(t, err, uniqueViolation, "period_gap_pkey")
	})

	t.Run("metric", func(t *testing.T) {
		insert := func(column string, key string) error {
			args := map[string]any{
				"user_id": user, "period_type": month, "period_key": key, "transfer_mode": withTransfers,
				"metric_id": "tiles.balance", "definition_version": int32(1), "status": int16(1),
			}
			if column != "" {
				args[column] = nil
			}
			_, err := db.Exec(ctx, `INSERT INTO period_metric
				(user_id, period_type, period_key, transfer_mode, metric_id, definition_version, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				args["user_id"], args["period_type"], args["period_key"], args["transfer_mode"], args["metric_id"],
				args["definition_version"], args["status"])
			return err
		}
		for _, column := range []string{"metric_id", "definition_version", "status"} {
			t.Run(column+" is required", func(t *testing.T) {
				wantViolation(t, insert(column, "2026-09"), notNullViolation, column)
			})
		}
		wantViolation(t, insert("", "2026-07"), foreignKeyViolation, "period_metric_period_snapshot_fkey")
		_, err := db.Exec(ctx, `INSERT INTO period_metric
			(user_id, period_type, period_key, transfer_mode, metric_id, definition_version, status) VALUES ($1, $2, $3, $4, $5, 1, 1)`,
			user, month, "2026-09", withTransfers, "tiles.expense")
		wantViolation(t, err, uniqueViolation, "period_metric_pkey")
	})

	t.Run("deleting a header deletes its gaps and metrics", func(t *testing.T) {
		if _, err := db.Exec(ctx, `DELETE FROM period_snapshot WHERE user_id = $1 AND period_key = $2`, user, "2026-09"); err != nil {
			t.Fatal(err)
		}
		var gaps, metrics, other int
		if err := db.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM period_gap WHERE user_id = $1 AND period_key = '2026-09'),
			(SELECT count(*) FROM period_metric WHERE user_id = $1 AND period_key = '2026-09'),
			(SELECT count(*) FROM period_metric WHERE user_id = $1 AND period_key = '2026')`, user).Scan(&gaps, &metrics, &other); err != nil {
			t.Fatal(err)
		}
		if gaps != 0 || metrics != 0 || other != 1 {
			t.Fatalf("after delete: %d gaps, %d metrics of the period, %d of another; want 0, 0, 1", gaps, metrics, other)
		}
	})

	// A key twice in one request (a period both sent and removed) fails the whole statement:
	// the handler validates the request before writing.
	_, err := upsertProto(ctx, db, user, 2, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-07", withTransfers, 2, "tiles.expense")},
		[]*analyticsv1.PeriodRef{ref(month, "2026-07")})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "21000" {
		t.Fatalf("duplicate key in one request: %v, want 21000 cardinality_violation", err)
	}
	if _, ok := storedPeriod(t, db, user, pk(month, "2026-07", withTransfers)); ok {
		t.Fatal("the failed statement wrote a row")
	}
}

func TestIntegrationStateAppliesByVersion(t *testing.T) {
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
	var version int32
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

func TestIntegrationPeriodsApplyByVersionPerKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	sep, aug, jul := pk(month, "2026-09", withTransfers), pk(month, "2026-08", withTransfers), pk(month, "2026-07", withTransfers)

	applied, err := upsertProto(ctx, db, user, 5, []*analyticsv1.PeriodSnapshot{
		snapshot(month, "2026-09", withTransfers, 5, "tiles.expense"),
		snapshot(month, "2026-08", withTransfers, 5, "tiles.expense"),
	}, nil)
	if err != nil || len(applied) != 2 {
		t.Fatalf("first snapshots: %v %v", applied, err)
	}

	// A request of an older version is skipped for the key it is older on, and applied for
	// a key it is the first for: the version is per key, not per user.
	applied, err = upsertProto(ctx, db, user, 4, []*analyticsv1.PeriodSnapshot{
		snapshot(month, "2026-09", withTransfers, 4, "tiles.expense", "tiles.income"),
		snapshot(month, "2026-07", withTransfers, 4, "tiles.expense"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].periodKey != jul {
		t.Fatalf("applied %v, want only 2026-07 (skipped_stale_periods = 1)", applied)
	}
	s, _ := storedPeriod(t, db, user, sep)
	if s.version != 5 || *s.unread != 5 {
		t.Fatalf("2026-09 is v%d with content of v%d after an older delivery, want v5", s.version, *s.unread)
	}
	wantMetrics(t, s, 5, "tiles.expense")
	if s, _ := storedPeriod(t, db, user, aug); s.version != 5 {
		t.Fatalf("2026-08 is v%d, want v5", s.version)
	}

	// The same version again (outbox redelivery) is applied and changes nothing.
	applied, err = upsertProto(ctx, db, user, 5, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withTransfers, 5, "tiles.expense")}, nil)
	if err != nil || len(applied) != 1 {
		t.Fatalf("redelivery: %v %v", applied, err)
	}
	s, _ = storedPeriod(t, db, user, sep)
	wantMetrics(t, s, 5, "tiles.expense")

	// The two transfer modes of one period are separate keys.
	if _, err := upsertProto(ctx, db, user, 6, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withoutTransfers, 6, "tiles.income")}, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = storedPeriod(t, db, user, sep)
	if s.version != 5 {
		t.Fatalf("mode with is v%d after a snapshot of mode without, want v5", s.version)
	}
	wantMetrics(t, s, 5, "tiles.expense")
}

// calc_version is int32 in the contract and integer in the schema: the largest version is
// stored and compared as is, and the one below it is skipped as older.
func TestIntegrationLargestCalcVersionRoundTrips(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	sep := pk(month, "2026-09", withTransfers)

	if err := deliver(ctx, db, user, math.MaxInt32, true,
		[]*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withTransfers, math.MaxInt32, "tiles.expense")}, nil); err != nil {
		t.Fatal(err)
	}
	var version int32
	if err := db.QueryRow(ctx, `SELECT calc_version FROM user_data_state WHERE user_id = $1`, user).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != math.MaxInt32 {
		t.Fatalf("state is v%d, want v%d", version, int32(math.MaxInt32))
	}
	s, ok := storedPeriod(t, db, user, sep)
	if !ok || s.version != math.MaxInt32 {
		t.Fatalf("2026-09 is v%d (present %v), want v%d", s.version, ok, int32(math.MaxInt32))
	}
	wantMetrics(t, s, math.MaxInt32, "tiles.expense")

	if ok, err := upsertState(ctx, db, user, math.MaxInt32-1, validState()); err != nil || ok {
		t.Fatalf("older state: applied %v, %v; want skipped", ok, err)
	}
	applied, err := upsertProto(ctx, db, user, math.MaxInt32-1,
		[]*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withTransfers, math.MaxInt32-1, "tiles.income")}, nil)
	if err != nil || len(applied) != 0 {
		t.Fatalf("older snapshot: applied %v, %v; want skipped", applied, err)
	}
	s, _ = storedPeriod(t, db, user, sep)
	wantMetrics(t, s, math.MaxInt32, "tiles.expense")
}

// The contract: a snapshot is the full set of metrics of its period; metrics absent from it are
// removed. Gaps are replaced the same way. A skipped (older) snapshot touches neither.
func TestIntegrationSnapshotReplacesMetricsAndGaps(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user, other := newUser(1), newUser(2)
	sep := pk(month, "2026-09", withTransfers)

	for _, u := range []string{user, other} {
		if _, err := upsertProto(ctx, db, u, 1, []*analyticsv1.PeriodSnapshot{
			withGaps(snapshot(month, "2026-09", withTransfers, 1, "tiles.expense", "tiles.income", "tiles.balance"), 3),
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := upsertProto(ctx, db, user, 2, []*analyticsv1.PeriodSnapshot{
		withGaps(snapshot(month, "2026-09", withTransfers, 2, "tiles.income", "insights.future_metric"), 1),
	}, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := storedPeriod(t, db, user, sep)
	wantMetrics(t, s, 2, "tiles.income", "insights.future_metric")
	if s.gaps != 1 {
		t.Fatalf("%d gaps, want 1", s.gaps)
	}

	// A snapshot with no metrics and no gaps leaves none.
	if _, err := upsertProto(ctx, db, user, 3, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withTransfers, 3)}, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = storedPeriod(t, db, user, sep)
	wantMetrics(t, s, 3)
	if s.gaps != 0 || *s.unread != 3 {
		t.Fatalf("%d gaps, content of v%d; want 0 and v3", s.gaps, *s.unread)
	}

	// An older snapshot is skipped and does not bring its metrics back.
	if _, err := upsertProto(ctx, db, user, 2, []*analyticsv1.PeriodSnapshot{
		withGaps(snapshot(month, "2026-09", withTransfers, 2, "tiles.expense"), 2),
	}, nil); err != nil {
		t.Fatal(err)
	}
	s, _ = storedPeriod(t, db, user, sep)
	wantMetrics(t, s, 3)
	if s.gaps != 0 || s.version != 3 {
		t.Fatalf("v%d with %d gaps after an older snapshot, want v3 with 0", s.version, s.gaps)
	}

	// Another user's period with the same key is untouched.
	s, _ = storedPeriod(t, db, other, sep)
	wantMetrics(t, s, 1, "tiles.expense", "tiles.income", "tiles.balance")
	if s.gaps != 3 {
		t.Fatalf("another user has %d gaps, want 3", s.gaps)
	}
}

func TestIntegrationRemovedPeriodStaysRemovedForOlderSnapshots(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	augWith, augWithout := pk(month, "2026-08", withTransfers), pk(month, "2026-08", withoutTransfers)

	if _, err := upsertProto(ctx, db, user, 3, []*analyticsv1.PeriodSnapshot{
		withGaps(snapshot(month, "2026-08", withTransfers, 3, "tiles.expense"), 1),
		snapshot(month, "2026-08", withoutTransfers, 3, "tiles.expense"),
	}, nil); err != nil {
		t.Fatal(err)
	}
	applied, err := upsertProto(ctx, db, user, 5, nil, []*analyticsv1.PeriodRef{ref(month, "2026-08")})
	if err != nil || len(applied) != 2 || !applied[0].removed || !applied[1].removed {
		t.Fatalf("removal: %v %v", applied, err)
	}
	for _, k := range []periodKey{augWith, augWithout} {
		s, ok := storedPeriod(t, db, user, k)
		if !ok || !s.removed || s.version != 5 || s.unread != nil || s.coverage != nil || len(s.metrics) != 0 || s.gaps != 0 {
			t.Fatalf("mode %d after removal: %+v (present %v), want a v5 marker without data, gaps and metrics", k.mode, s, ok)
		}
	}

	// A snapshot of version 4, delivered after the removal of version 5, must not bring the
	// period back: the marker keeps version 5, and no metrics are written for it.
	applied, err = upsertProto(ctx, db, user, 4, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-08", withTransfers, 4, "tiles.expense")}, nil)
	if err != nil || len(applied) != 0 {
		t.Fatalf("late snapshot after removal: applied %v, %v; want skipped", applied, err)
	}
	if s, ok := storedPeriod(t, db, user, augWith); !ok || !s.removed || s.version != 5 || len(s.metrics) != 0 {
		t.Fatalf("2026-08 is %+v (present %v), want a v5 removal marker", s, ok)
	}
	var latest *string
	if err := db.QueryRow(ctx, latestPeriodSQL, user, month).Scan(&latest); err != nil || latest != nil {
		t.Fatalf("latest period %v, %v; want none: markers are not periods with data", latest, err)
	}

	// A removal of an older version does not remove a newer snapshot. The removal is compared
	// with the greater version of both modes (grpc-api.md 7.3, «Версии»): with the v7 snapshot in
	// one mode and the v5 marker in the other, the v6 removal is skipped whole — the marker of the
	// other mode is not raised to v6 either.
	if _, err := upsertProto(ctx, db, user, 7, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-08", withTransfers, 7, "tiles.expense")}, nil); err != nil {
		t.Fatal(err)
	}
	applied, err = upsertProto(ctx, db, user, 6, nil, []*analyticsv1.PeriodRef{ref(month, "2026-08")})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("older removal applied to %v, want it skipped in both modes", applied)
	}
	s, _ := storedPeriod(t, db, user, augWith)
	if s.removed || s.version != 7 {
		t.Fatalf("2026-08 is v%d removed %v after an older removal, want the v7 snapshot", s.version, s.removed)
	}
	wantMetrics(t, s, 7, "tiles.expense")
	if s, _ := storedPeriod(t, db, user, augWithout); !s.removed || s.version != 5 {
		t.Fatalf("mode without is v%d removed %v, want the v5 marker untouched", s.version, s.removed)
	}

	// A removal of the same version as the newest row of the period is applied to both modes.
	applied, err = upsertProto(ctx, db, user, 7, nil, []*analyticsv1.PeriodRef{ref(month, "2026-08")})
	if err != nil || len(applied) != 2 {
		t.Fatalf("removal of the newest version: %v %v; want both modes", applied, err)
	}
	for _, k := range []periodKey{augWith, augWithout} {
		if s, _ := storedPeriod(t, db, user, k); !s.removed || s.version != 7 || len(s.metrics) != 0 {
			t.Fatalf("mode %d: %+v, want a v7 marker", k.mode, s)
		}
	}

	// A removal of a period the service never saw leaves markers too.
	if applied, err := upsertProto(ctx, db, user, 8, nil, []*analyticsv1.PeriodRef{ref(month, "2025-01")}); err != nil || len(applied) != 2 {
		t.Fatalf("removal of an unknown period: %v %v", applied, err)
	}
}

// A marker (tombstone of the period) only grows: a removal older than an existing marker is
// skipped and a redelivered old removal does not lower it, also when only one mode has a
// marker and the other a newer snapshot (contracts#60, FIN-283).
func TestIntegrationRemovalMarkerVersionOnlyGrows(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	julWith, julWithout := pk(month, "2026-07", withTransfers), pk(month, "2026-07", withoutTransfers)

	if applied, err := upsertProto(ctx, db, user, 5, nil, []*analyticsv1.PeriodRef{ref(month, "2026-07")}); err != nil || len(applied) != 2 {
		t.Fatalf("removal v5: %v %v", applied, err)
	}
	for _, version := range []int32{4, 3, 4} {
		if applied, err := upsertProto(ctx, db, user, version, nil, []*analyticsv1.PeriodRef{ref(month, "2026-07")}); err != nil || len(applied) != 0 {
			t.Fatalf("removal v%d after the v5 markers: %v %v; want skipped", version, applied, err)
		}
	}
	for _, k := range []periodKey{julWith, julWithout} {
		if s, _ := storedPeriod(t, db, user, k); !s.removed || s.version != 5 {
			t.Fatalf("mode %d: v%d removed %v, want the v5 marker", k.mode, s.version, s.removed)
		}
	}

	// A period sent again in one mode only: v6 snapshot in mode without, the v5 marker stays in
	// mode with. A removal v5 or older is skipped in both modes; v6 removes both.
	if applied, err := upsertProto(ctx, db, user, 6, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-07", withoutTransfers, 6, "tiles.income")}, nil); err != nil || len(applied) != 1 {
		t.Fatalf("one-mode snapshot v6: %v %v", applied, err)
	}
	if applied, err := upsertProto(ctx, db, user, 4, nil, []*analyticsv1.PeriodRef{ref(month, "2026-07")}); err != nil || len(applied) != 0 {
		t.Fatalf("removal v4 with a marker in one mode only: %v %v; want skipped", applied, err)
	}
	if s, _ := storedPeriod(t, db, user, julWith); !s.removed || s.version != 5 {
		t.Fatalf("mode with: v%d removed %v, want the v5 marker", s.version, s.removed)
	}
	s, _ := storedPeriod(t, db, user, julWithout)
	if s.removed || s.version != 6 {
		t.Fatalf("mode without: v%d removed %v, want the v6 snapshot", s.version, s.removed)
	}
	wantMetrics(t, s, 6, "tiles.income")
	if applied, err := upsertProto(ctx, db, user, 6, nil, []*analyticsv1.PeriodRef{ref(month, "2026-07")}); err != nil || len(applied) != 2 {
		t.Fatalf("removal v6: %v %v; want both modes", applied, err)
	}
	// A snapshot older than the v6 marker does not come back in either mode.
	if applied, err := upsertProto(ctx, db, user, 5, both(month, "2026-07", 5, "tiles.expense"), nil); err != nil || len(applied) != 0 {
		t.Fatalf("snapshots v5 after the v6 markers: %v %v; want skipped", applied, err)
	}
}

// One request: the state first, then the periods, in one transaction — the order the handler
// is expected to use.
func deliver(ctx context.Context, db *pgxpool.Pool, user string, version int32, withState bool,
	snapshots []*analyticsv1.PeriodSnapshot, removed []*analyticsv1.PeriodRef,
) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if withState {
		s := validState()
		s.fullMonths = version
		if _, err := upsertState(ctx, tx, user, version, s); err != nil {
			return fmt.Errorf("state v%d: %w", version, err)
		}
	}
	if _, err := upsertProto(ctx, tx, user, version, snapshots, removed); err != nil {
		return fmt.Errorf("periods v%d: %w", version, err)
	}
	return tx.Commit(ctx)
}

// metricsOfVersion is the metric set request v sends: it differs between versions, so a key
// that kept metrics of another version, or both sets mixed, is caught.
func metricsOfVersion(v int32) []string {
	ids := []string{"tiles.expense", fmt.Sprintf("tiles.v%d", v%3)}
	if v%2 == 0 {
		ids = append(ids, "insights.bank_fees")
	}
	return ids
}

// Concurrent and out-of-order delivery: several requests of one user race on the same keys,
// in shuffled order, half of them without the state (so they do not serialise on its row),
// some removing a period the others send, each with its own set of metrics and gaps.
// Whatever the interleaving: no deadlock, no error, and every key ends at the highest version
// with exactly that version's metrics and gaps.
func TestIntegrationConcurrentDeliveryKeepsHighestVersion(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	keys := []string{"2026-05", "2026-06", "2026-07", "2026-08", "2026-09"}
	const writers = 8

	for round := range 15 {
		user := newUser(1000 + round)
		// What request v did: removed 2026-07 when v is divisible by 3, otherwise sent it.
		removes := func(v int32) bool { return v%3 == 0 }
		start := make(chan struct{})
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for _, v := range rand.Perm(writers) {
			version := int32(v + 1)
			var snapshots []*analyticsv1.PeriodSnapshot
			var removed []*analyticsv1.PeriodRef
			for _, k := range keys {
				if k == "2026-07" && removes(version) {
					removed = append(removed, ref(month, k))
					continue
				}
				for _, s := range both(month, k, version, metricsOfVersion(version)...) {
					snapshots = append(snapshots, withGaps(s, int(version%5)))
				}
			}
			rand.Shuffle(len(snapshots), func(i, j int) { snapshots[i], snapshots[j] = snapshots[j], snapshots[i] })
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- deliver(ctx, db, user, version, version%2 == 0, snapshots, removed)
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
				s, ok := storedPeriod(t, db, user, pk(month, k, mode))
				if !ok || s.version != writers {
					t.Fatalf("round %d: %s mode %d is v%d (present %v), want v%d", round, k, mode, s.version, ok, writers)
				}
				if k == "2026-07" && removes(writers) {
					if !s.removed || len(s.metrics) != 0 || s.gaps != 0 {
						t.Fatalf("round %d: %s mode %d is %+v, want a removal marker", round, k, mode, s)
					}
					continue
				}
				if s.removed || *s.unread != writers || s.gaps != writers%5 {
					t.Fatalf("round %d: %s mode %d is %+v, want the content and %d gaps of v%d", round, k, mode, s, writers%5, writers)
				}
				wantMetrics(t, s, writers, metricsOfVersion(writers)...)
			}
		}
		var version int32
		if err := db.QueryRow(ctx, `SELECT calc_version FROM user_data_state WHERE user_id = $1`, user).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != writers {
			t.Fatalf("round %d: state is v%d, want v%d", round, version, writers)
		}
	}
}

func timeRange(start, end time.Time) *commonv1.TimeRange {
	return &commonv1.TimeRange{Start: at(start), End: at(end)}
}

// richSnapshot uses every column and every kind of details, with the edge values a column
// mapping can lose: zero next to absent, negative and maximal amounts, an empty details message,
// a basis without a range, an unknown metric_id with an unknown status number.
func richSnapshot() *analyticsv1.PeriodSnapshot {
	start, end := periodRange(month, "2026-09")
	aug, _ := periodRange(month, "2026-08")
	jun, _ := periodRange(month, "2026-06")
	sep25 := time.Date(2026, 9, 25, 0, 0, 0, 0, msk)
	sepFilter := func(kind commonv1.KindFilter) *commonv1.OperationsFilter {
		return &commonv1.OperationsFilter{From: at(start), To: at(end), Kind: kind, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH}
	}
	metric := func(id string, m *analyticsv1.Metric) *analyticsv1.MetricSnapshot {
		return &analyticsv1.MetricSnapshot{MetricId: id, DefinitionVersion: 2, Metric: m}
	}
	ready, tentative := analyticsv1.MetricStatus_METRIC_STATUS_READY, analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE
	locked, none := analyticsv1.MetricStatus_METRIC_STATUS_LOCKED, analyticsv1.MetricStatus_METRIC_STATUS_NONE
	return &analyticsv1.PeriodSnapshot{
		Period:       commonv1.PeriodType_PERIOD_TYPE_MONTH,
		PeriodKey:    "2026-09",
		TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH,
		Range:        timeRange(start, end),
		Bounds: &commonv1.DataBounds{
			Coverage: commonv1.Coverage_COVERAGE_PARTIAL,
			DataFrom: at(start),
			DataTo:   at(time.Date(2026, 9, 25, 21, 40, 0, 0, msk)),
			Gaps: []*commonv1.TimeRange{
				timeRange(start.AddDate(0, 0, 3), start.AddDate(0, 0, 5)),
				timeRange(start.AddDate(0, 0, 10), start.AddDate(0, 0, 11)),
			},
		},
		UnreadLinesCount: 5,
		CalculatedAt:     at(time.Date(2026, 9, 25, 21, 52, 7, 123456000, msk)),
		// Not in metric_id order: the read returns them sorted.
		Metrics: []*analyticsv1.MetricSnapshot{
			metric("tiles.expense", &analyticsv1.Metric{Status: ready, ValueMinor: proto.Int64(8432000),
				Filters: sepFilter(commonv1.KindFilter_KIND_FILTER_EXPENSE)}),
			metric("tiles.expense.comparison", &analyticsv1.Metric{Status: ready, ValueMinor: proto.Int64(0),
				Range: timeRange(aug, aug.AddDate(0, 0, 25))}),
			metric("tiles.expense.typical", &analyticsv1.Metric{Status: tentative, ValueMinor: proto.Int64(-150),
				Basis: &analyticsv1.MetricBasis{Range: timeRange(jun, start), FullMonths: 2}}),
			metric("tiles.daily_expense", &analyticsv1.Metric{Status: ready, ValueMinor: proto.Int64(9223372036854775807)}),
			metric("tiles.income", &analyticsv1.Metric{Status: ready}),
			metric("tiles.forecast", &analyticsv1.Metric{Status: locked, Lock: &analyticsv1.MetricLock{
				Reason: analyticsv1.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH, AvailableFrom: at(time.Date(2026, 10, 10, 0, 0, 0, 0, msk))}}),
			metric("tiles.balance", &analyticsv1.Metric{Status: ready, ValueMinor: proto.Int64(4000),
				Details: &analyticsv1.Metric_Balance{Balance: &analyticsv1.BalanceDetails{ShareOfIncome: proto.Float64(0.46)}}}),
			metric("expense_categories", &analyticsv1.Metric{Status: ready, Details: &analyticsv1.Metric_Categories{Categories: &analyticsv1.CategoryBreakdown{
				Items: []*analyticsv1.CategoryAmount{
					{CategoryId: proto.String("cat_groceries"), CategoryName: "Супермаркеты", CategoryIcon: "cart", Note: proto.String("5 переводов"),
						AmountMinor: 5200000, Share: proto.Float64(1.0 / 3), OperationCount: 42,
						Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), CategoryId: proto.String("cat_groceries"),
							Kind: commonv1.KindFilter_KIND_FILTER_EXPENSE, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH}},
					{CategoryName: "Без категории", CategoryIcon: "question", AmountMinor: -1200, OperationCount: 1,
						Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), Selection: proto.String("sel_uncategorized")}},
				},
			}}}),
			metric("income_categories", &analyticsv1.Metric{Status: none, Basis: &analyticsv1.MetricBasis{FullMonths: 0}}),
			metric("monthly_chart", &analyticsv1.Metric{Status: ready, Details: &analyticsv1.Metric_MonthlyChart{MonthlyChart: &analyticsv1.MonthlyChart{
				Points: []*analyticsv1.MonthlyPoint{
					{Month: "2026-08", Range: timeRange(aug, start), IncomeMinor: proto.Int64(0),
						Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_NO_DATA}},
					{Month: "2026-09", Range: timeRange(start, end), ExpenseMinor: proto.Int64(8432000), IncomeMinor: proto.Int64(12000000),
						Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: at(start), DataTo: at(sep25),
							Gaps: []*commonv1.TimeRange{timeRange(start.AddDate(0, 0, 3), start.AddDate(0, 0, 5))}}},
				},
			}}}),
			metric("insights.regular_payments", &analyticsv1.Metric{Status: tentative, ValueMinor: proto.Int64(129900),
				Basis: &analyticsv1.MetricBasis{Range: timeRange(jun, start), FullMonths: 2},
				Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), Selection: proto.String("sel_regular_all"),
					TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH},
				Details: &analyticsv1.Metric_RegularPayments{RegularPayments: &analyticsv1.RegularPayments{Count: proto.Int32(2), Items: []*analyticsv1.RegularPayment{
					{Id: "01a107db-ae80-7b07-b8a1-000000000001", Title: "Спортзал", AmountMinor: 99900, MonthlyAmountMinor: 99900,
						ScheduleName: "15-го числа", CategoryId: "cat_sport",
						Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), Selection: proto.String("sel_payment_1"), SelectionName: proto.String("Спортзал")}},
				}}}}),
			metric("insights.notable_spending", &analyticsv1.Metric{Status: locked, Lock: &analyticsv1.MetricLock{
				Reason: analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS, Required: proto.Int32(3), Available: proto.Int32(0)},
				Details: &analyticsv1.Metric_NotableSpending{NotableSpending: &analyticsv1.NotableSpending{Items: []*analyticsv1.NotableSpendingItem{
					{CategoryId: "cat_cafe", CategoryName: "Кафе", CategoryIcon: "cup", AmountMinor: 300000, TypicalAmountMinor: 120000,
						Filters: sepFilter(commonv1.KindFilter_KIND_FILTER_EXPENSE)},
				}}}}),
			metric("insights.bank_fees", &analyticsv1.Metric{Status: none,
				Details: &analyticsv1.Metric_BankFees{BankFees: &analyticsv1.BankFees{}}}),
			metric("insights.small_frequent", &analyticsv1.Metric{Status: ready, ValueMinor: proto.Int64(640000),
				Basis: &analyticsv1.MetricBasis{Range: timeRange(aug, start), FullMonths: 1},
				Details: &analyticsv1.Metric_SmallFrequent{SmallFrequent: &analyticsv1.SmallFrequent{Items: []*analyticsv1.SmallFrequentGroup{
					{Title: "Кофе и перекусы", Count: 16, AverageAmountMinor: 40000, MonthlyAmountMinor: 640000, YearlyAmountMinor: 7680000,
						Filters: &commonv1.OperationsFilter{From: at(aug), To: at(start), Selection: proto.String("sel_group_1")}},
				}}}}),
			// A metric of a newer registry and a status number this build does not know.
			metric("insights.future_metric", &analyticsv1.Metric{Status: analyticsv1.MetricStatus(9), ValueMinor: proto.Int64(1),
				Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), CategoryId: proto.String("cat_new"),
					Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT,
					Selection: proto.String("sel_merchant"), SelectionName: proto.String("Продавец")}}),
		},
	}
}

// sortedByMetricID is what the read returns for s: the same snapshot, metrics in metric_id order.
func sortedByMetricID(s *analyticsv1.PeriodSnapshot) *analyticsv1.PeriodSnapshot {
	out := proto.CloneOf(s)
	slices.SortFunc(out.Metrics, func(a, b *analyticsv1.MetricSnapshot) int { return strings.Compare(a.GetMetricId(), b.GetMetricId()) })
	return out
}

func writeAndRead(t *testing.T, db *pgxpool.Pool, user string, s *analyticsv1.PeriodSnapshot) *analyticsv1.PeriodSnapshot {
	t.Helper()
	ctx := context.Background()
	if _, err := upsertState(ctx, db, user, 1, validState()); err != nil {
		t.Fatal(err)
	}
	if applied, err := upsertProto(ctx, db, user, 1, []*analyticsv1.PeriodSnapshot{s}, nil); err != nil || len(applied) != 1 {
		t.Fatalf("write: %v %v", applied, err)
	}
	got, err := readAnalytics(ctx, db, user, protoKey(s))
	if err != nil {
		t.Fatal(err)
	}
	if got.snapshot == nil {
		t.Fatal("no snapshot read back")
	}
	return got.snapshot
}

// A snapshot written to the normalised rows and read back is the same PeriodSnapshot: every
// column, every kind of details, gaps in order, presence of optional fields.
func TestIntegrationSnapshotRoundTrip(t *testing.T) {
	db := openDB(t)

	t.Run("every column and every kind of details", func(t *testing.T) {
		in := richSnapshot()
		got := writeAndRead(t, db, newUser(1), in)
		if want := sortedByMetricID(in); !proto.Equal(got, want) {
			t.Fatalf("snapshot came back as\n%v\nwant\n%v", got, want)
		}
		// The checks proto.Equal makes, spelled out for the cases a mapping loses first.
		byID := map[string]*analyticsv1.Metric{}
		for _, m := range got.GetMetrics() {
			byID[m.GetMetricId()] = m.GetMetric()
		}
		if v := byID["tiles.expense.comparison"].ValueMinor; v == nil || *v != 0 {
			t.Fatalf("value_minor 0 came back as %v, want 0 (not absent)", v)
		}
		if v := byID["tiles.income"].ValueMinor; v != nil {
			t.Fatalf("absent value_minor came back as %d", *v)
		}
		points := byID["monthly_chart"].GetMonthlyChart().GetPoints()
		if points[0].ExpenseMinor != nil || points[0].IncomeMinor == nil || *points[0].IncomeMinor != 0 {
			t.Fatalf("monthly point: expense %v (want absent = no data), income %v (want 0)", points[0].ExpenseMinor, points[0].IncomeMinor)
		}
		if byID["insights.bank_fees"].GetBankFees() == nil {
			t.Fatal("empty bank_fees details came back as no details")
		}
		if byID["tiles.expense"].GetDetails() != nil {
			t.Fatal("a metric without details came back with details")
		}

		// Gaps come back in ordinal order, not in the physical order of their rows: rewrite them
		// newest first and read again without the index, which would return them sorted anyway.
		ctx := context.Background()
		if _, err := db.Exec(ctx, `
			WITH old AS (DELETE FROM period_gap WHERE user_id = $1 RETURNING *)
			INSERT INTO period_gap SELECT * FROM old ORDER BY ordinal DESC`, newUser(1)); err != nil {
			t.Fatal(err)
		}
		conn, err := db.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SET enable_indexscan = off; SET enable_bitmapscan = off`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = conn.Exec(ctx, `RESET enable_indexscan; RESET enable_bitmapscan`) }()
		again, err := readAnalytics(ctx, conn, newUser(1), protoKey(in))
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(again.snapshot.GetBounds(), in.GetBounds()) {
			t.Fatalf("bounds after the gaps were rewritten in reverse: %v, want %v", again.snapshot.GetBounds(), in.GetBounds())
		}
	})

	t.Run("a complete snapshot without metrics and gaps", func(t *testing.T) {
		in := completeSnapshotWithoutMetrics()
		if got := writeAndRead(t, db, newUser(2), in); !proto.Equal(got, in) {
			t.Fatalf("snapshot came back as %v, want %v", got, in)
		}
	})
}

// What the normalised storage does not keep, by design (docs/schema.md, «Альтернативы»): the
// lock text (analytics builds it), unknown protobuf fields, nanoseconds of a moment. And what it
// tolerates: details written by a newer release with keys this build does not know.
func TestIntegrationRoundTripDropsWhatIsNotStored(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	unknown := protowire.AppendString(protowire.AppendTag(nil, 99, protowire.BytesType), "from a newer producer")
	in := snapshot(month, "2026-09", withTransfers, 1, "insights.notable_spending", "expense_categories")
	in.ProtoReflect().SetUnknown(unknown)
	in.CalculatedAt = at(time.Date(2026, 9, 25, 21, 52, 7, 123456789, msk))
	in.Metrics[0].Metric = &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_LOCKED, Lock: &analyticsv1.MetricLock{
		Reason: analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS, Required: proto.Int32(3), Available: proto.Int32(1),
		Text: "Нужно 3 полных месяца",
	}}
	categories := &analyticsv1.CategoryBreakdown{}
	categories.ProtoReflect().SetUnknown(unknown)
	in.Metrics[1].Metric = &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_NONE,
		Details: &analyticsv1.Metric_Categories{Categories: categories}}

	got := writeAndRead(t, db, newUser(1), in)

	want := sortedByMetricID(in)
	want.ProtoReflect().SetUnknown(nil)
	want.CalculatedAt = at(time.Date(2026, 9, 25, 21, 52, 7, 123456000, msk))
	for _, m := range want.Metrics {
		if l := m.GetMetric().GetLock(); l != nil {
			l.Text = ""
		}
		if c := m.GetMetric().GetCategories(); c != nil {
			c.ProtoReflect().SetUnknown(nil)
		}
	}
	if !proto.Equal(got, want) {
		t.Fatalf("snapshot came back as\n%v\nwant\n%v", got, want)
	}

	// A newer release wrote a details variant and a field this build does not know.
	if _, err := db.Exec(ctx, `UPDATE period_metric SET details = $2 WHERE user_id = $1 AND metric_id = 'expense_categories'`,
		newUser(1), `{"categories": {"items": [{"category_name": "Кафе", "amount_minor": "100", "future_field": 1}]}, "future_kind": {}}`); err != nil {
		t.Fatal(err)
	}
	read, err := readAnalytics(ctx, db, newUser(1), protoKey(in))
	if err != nil {
		t.Fatalf("read of details with unknown keys: %v", err)
	}
	for _, m := range read.snapshot.GetMetrics() {
		if m.GetMetricId() == "expense_categories" {
			items := m.GetMetric().GetCategories().GetItems()
			if len(items) != 1 || items[0].GetCategoryName() != "Кафе" || items[0].GetAmountMinor() != 100 {
				t.Fatalf("categories %v, want the known part of the newer details", items)
			}
		}
	}
}

// The mapping refuses what it cannot store as received instead of storing something else: an
// enum number beyond smallint, a gap without a bound. Nothing of the request is written. And a
// header broken by hand (removed = false without its data) fails the read instead of a panic.
func TestIntegrationMappingRefusesWhatItCannotStore(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	sep := pk(month, "2026-09", withTransfers)

	cases := []struct {
		name   string
		change func(s *analyticsv1.PeriodSnapshot)
		check  func(t *testing.T, err error)
	}{
		{"status beyond smallint", func(s *analyticsv1.PeriodSnapshot) {
			s.Metrics[0].Metric.Status = analyticsv1.MetricStatus(65537)
		}, wantEnumRange},
		{"lock reason beyond smallint", func(s *analyticsv1.PeriodSnapshot) {
			s.Metrics[0].Metric.Lock = &analyticsv1.MetricLock{Reason: analyticsv1.LockReason(-40000)}
		}, wantEnumRange},
		{"filter kind beyond smallint", func(s *analyticsv1.PeriodSnapshot) {
			s.Metrics[0].Metric.Filters = &commonv1.OperationsFilter{From: s.GetRange().GetStart(), To: s.GetRange().GetEnd(), Kind: commonv1.KindFilter(32768)}
		}, wantEnumRange},
		{"coverage beyond smallint", func(s *analyticsv1.PeriodSnapshot) {
			s.Bounds.Coverage = commonv1.Coverage(1 << 20)
		}, wantEnumRange},
		{"gap without its end", func(s *analyticsv1.PeriodSnapshot) {
			s.Bounds.Gaps[0].End = nil
		}, func(t *testing.T, err error) { wantViolation(t, err, notNullViolation, "gap_end") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := withGaps(snapshot(month, "2026-09", withTransfers, 1, "tiles.expense"), 1)
			c.change(s)
			c.check(t, deliver(ctx, db, user, 1, true, []*analyticsv1.PeriodSnapshot{s}, nil))
			if _, ok := storedPeriod(t, db, user, sep); ok {
				t.Fatal("the refused request wrote its header")
			}
		})
	}

	t.Run("a header without its data fails the read", func(t *testing.T) {
		if err := deliver(ctx, db, user, 1, true, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-09", withTransfers, 1, "tiles.expense")}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE period_snapshot SET unread_lines_count = NULL WHERE user_id = $1`, user); err != nil {
			t.Fatal(err)
		}
		if _, err := readAnalytics(ctx, db, user, sep); !errors.Is(err, errBrokenHeader) {
			t.Fatalf("read of a header without unread_lines_count: %v, want %v", err, errBrokenHeader)
		}
		if _, err := db.Exec(ctx, `UPDATE period_snapshot SET unread_lines_count = 1, calculated_at = NULL WHERE user_id = $1`, user); err != nil {
			t.Fatal(err)
		}
		if _, err := readAnalytics(ctx, db, user, sep); !errors.Is(err, errBrokenHeader) {
			t.Fatalf("read of a header without calculated_at: %v, want %v", err, errBrokenHeader)
		}
	})
}

func wantEnumRange(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errEnumRange) {
		t.Fatalf("error %v, want %v", err, errEnumRange)
	}
}

func seedForReads(t *testing.T, db *pgxpool.Pool, user string) {
	t.Helper()
	ctx := context.Background()
	if _, err := upsertState(ctx, db, user, 10, validState()); err != nil {
		t.Fatal(err)
	}
	var snapshots []*analyticsv1.PeriodSnapshot
	for _, k := range []string{"2026-05", "2026-06", "2026-08", "2026-09"} {
		snapshots = append(snapshots,
			withGaps(snapshot(month, k, withTransfers, 10, "tiles.expense", "tiles.income"), 1),
			snapshot(month, k, withoutTransfers, 10, "tiles.expense"))
	}
	snapshots = append(snapshots, snapshot(quarter, "2026-Q2", withTransfers, 10, "tiles.expense"), snapshot(year, "2026", withTransfers, 10))
	if _, err := upsertProto(ctx, db, user, 10, snapshots, []*analyticsv1.PeriodRef{ref(month, "2026-07")}); err != nil {
		t.Fatal(err)
	}
	// 2026-06 is complete in both modes.
	if _, err := db.Exec(ctx, `UPDATE period_snapshot SET coverage = $3 WHERE user_id = $1 AND period_key = $2`, user, "2026-06", complete); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationReadQueries(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user, other := newUser(1), newUser(2)
	seedForReads(t, db, user)
	seedForReads(t, db, other)
	if _, err := upsertProto(ctx, db, other, 11, []*analyticsv1.PeriodSnapshot{snapshot(month, "2026-10", withTransfers, 11, "tiles.expense")}, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("latest period", func(t *testing.T) {
		var latest string
		if err := db.QueryRow(ctx, latestPeriodSQL, user, month).Scan(&latest); err != nil || latest != "2026-09" {
			t.Fatalf("latest %q, %v; want 2026-09 (2026-10 belongs to another user)", latest, err)
		}
	})

	t.Run("analytics for a period with navigation over a removed one", func(t *testing.T) {
		got, err := readAnalytics(ctx, db, user, pk(month, "2026-08", withoutTransfers))
		if err != nil {
			t.Fatal(err)
		}
		if !got.state.hasData || got.state.timeZone != "Europe/Moscow" || got.state.fullMonths != 0 {
			t.Fatalf("state %+v", got.state)
		}
		want := snapshot(month, "2026-08", withoutTransfers, 10, "tiles.expense")
		if !proto.Equal(got.snapshot, want) {
			t.Fatalf("snapshot %v, want the mode without: %v", got.snapshot, want)
		}
		if deref(got.previous) != "2026-06" || deref(got.next) != "2026-09" {
			t.Fatalf("navigation %s / %s, want 2026-06 (2026-07 removed) / 2026-09", deref(got.previous), deref(got.next))
		}
		with, err := readAnalytics(ctx, db, user, pk(month, "2026-08", withTransfers))
		if err != nil {
			t.Fatal(err)
		}
		if want := withGaps(snapshot(month, "2026-08", withTransfers, 10, "tiles.expense", "tiles.income"), 1); !proto.Equal(with.snapshot, sortedByMetricID(want)) {
			t.Fatalf("snapshot %v, want the mode with: %v", with.snapshot, want)
		}
	})

	t.Run("analytics for a removed period and an unknown user", func(t *testing.T) {
		got, err := readAnalytics(ctx, db, user, pk(month, "2026-07", withTransfers))
		if err != nil || got.snapshot != nil || deref(got.previous) != "2026-06" || deref(got.next) != "2026-08" {
			t.Fatalf("removed period: snapshot %v navigation %s / %s, %v", got.snapshot, deref(got.previous), deref(got.next), err)
		}
		if !got.state.hasData {
			t.Fatal("the state is read without a snapshot")
		}
		if _, err := readAnalytics(ctx, db, newUser(99), pk(month, "2026-09", withTransfers)); !errors.Is(err, errNoState) {
			t.Fatalf("unknown user: %v, want no rows (has_data = false)", err)
		}
	})

	t.Run("a snapshot without metrics", func(t *testing.T) {
		got, err := readAnalytics(ctx, db, user, pk(year, "2026", withTransfers))
		if err != nil || got.snapshot == nil || len(got.snapshot.GetMetrics()) != 0 {
			t.Fatalf("year 2026: %v, %v; want a snapshot without metrics", got.snapshot, err)
		}
	})

	t.Run("first and last period have no neighbour", func(t *testing.T) {
		first, err := readAnalytics(ctx, db, user, pk(month, "2026-05", withTransfers))
		if err != nil || first.previous != nil {
			t.Fatalf("2026-05 previous %s, %v; want none", deref(first.previous), err)
		}
		last, err := readAnalytics(ctx, db, user, pk(month, "2026-09", withTransfers))
		if err != nil || last.next != nil {
			t.Fatalf("2026-09 next %s, %v; want none (2026-10 belongs to another user)", deref(last.next), err)
		}
	})

	t.Run("list periods", func(t *testing.T) {
		got, err := listPeriods(ctx, db, user, domain.PeriodMonth)
		if err != nil {
			t.Fatal(err)
		}
		type item struct {
			key      string
			coverage int16
		}
		var items []item
		for _, p := range got.Items {
			items = append(items, item{p.Key, int16(p.Coverage)})
		}
		want := []item{{"2026-09", partial}, {"2026-08", partial}, {"2026-06", complete}, {"2026-05", partial}}
		if !slices.Equal(items, want) || got.TimeZone != "Europe/Moscow" {
			t.Fatalf("periods %v in %q, want %v in Europe/Moscow: newest first, one per period, no removed ones", items, got.TimeZone, want)
		}
		start, end := periodRange(month, "2026-09")
		if !got.Items[0].Start.Equal(start) || !got.Items[0].End.Equal(end) {
			t.Fatalf("range of 2026-09: %v–%v, want %v–%v", got.Items[0].Start, got.Items[0].End, start, end)
		}
		// Another type has its own periods; a user without state or periods gets nothing.
		if q, err := listPeriods(ctx, db, user, domain.PeriodQuarter); err != nil || len(q.Items) != 1 || q.Items[0].Key != "2026-Q2" {
			t.Fatalf("quarters %+v, %v; want 2026-Q2", q, err)
		}
		if none, err := listPeriods(ctx, db, newUser(99), domain.PeriodMonth); err != nil || none.TimeZone != "" || len(none.Items) != 0 {
			t.Fatalf("unknown user: %+v, %v; want nothing", none, err)
		}
	})

	t.Run("purge removes only this user", func(t *testing.T) {
		if _, err := db.Exec(ctx, purgeSQL, user); err != nil {
			t.Fatal(err)
		}
		count := `SELECT (SELECT count(*) FROM period_snapshot WHERE user_id = $1) + (SELECT count(*) FROM period_gap WHERE user_id = $1)
			+ (SELECT count(*) FROM period_metric WHERE user_id = $1) + (SELECT count(*) FROM user_data_state WHERE user_id = $1)`
		var mine, theirs int
		if err := db.QueryRow(ctx, count, user).Scan(&mine); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, count, other).Scan(&theirs); err != nil {
			t.Fatal(err)
		}
		if mine != 0 || theirs == 0 {
			t.Fatalf("after purge: %d rows of the user, %d of another; want 0 and all", mine, theirs)
		}
	})
}

// Navigation, the latest period and ListPeriods across a year boundary for every period type.
// For keys of the contract grammar (docs/schema.md, «Инварианты в коде сервиса») byte order in
// COLLATE "C" is calendar order; the year boundary is where a wrong order would show first.
// Keys of that grammar sort the same in any sane collation, so the collation of the key columns
// is asserted from the catalog: the order of the index must not depend on the database locale.
func TestIntegrationReadQueriesAcrossYearBoundary(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	user := newUser(1)
	if _, err := upsertState(ctx, db, user, 1, validState()); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(ctx, `SELECT a.attrelid::regclass::text || '.' || a.attname, c.collname
		FROM pg_attribute a JOIN pg_collation c ON c.oid = a.attcollation
		WHERE a.attrelid IN ('period_snapshot'::regclass, 'period_gap'::regclass, 'period_metric'::regclass)
		  AND a.attname IN ('period_key', 'metric_id')`)
	if err != nil {
		t.Fatal(err)
	}
	collations, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var c [2]string
		return c, r.Scan(&c[0], &c[1])
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collations) != 4 {
		t.Fatalf("key columns %v, want period_key of three tables and metric_id", collations)
	}
	for _, c := range collations {
		if c[1] != "C" {
			t.Fatalf("%s collation %q, want C", c[0], c[1])
		}
	}

	cases := []struct {
		name           string
		typ            int16
		older, newer   string
		removedAfter   string // a removal marker later than newer: not a period with data
		olderInTheYear string // an earlier key of the older year, inserted last
	}{
		{"month", month, "2025-12", "2026-01", "2026-02", "2025-11"},
		{"quarter", quarter, "2025-Q4", "2026-Q1", "2026-Q2", "2025-Q3"},
		{"year", year, "2025", "2026", "2027", "2024"},
	}
	for _, c := range cases {
		// Newest first, so the stored order is not the insertion order.
		snapshots := []*analyticsv1.PeriodSnapshot{
			snapshot(c.typ, c.newer, withTransfers, 1, "tiles.expense"),
			snapshot(c.typ, c.older, withTransfers, 1, "tiles.expense"),
			snapshot(c.typ, c.olderInTheYear, withTransfers, 1, "tiles.expense"),
		}
		if _, err := upsertProto(ctx, db, user, 1, snapshots, []*analyticsv1.PeriodRef{ref(c.typ, c.removedAfter)}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var latest string
			if err := db.QueryRow(ctx, latestPeriodSQL, user, c.typ).Scan(&latest); err != nil || latest != c.newer {
				t.Fatalf("latest %q, %v; want %s", latest, err, c.newer)
			}

			got, err := readAnalytics(ctx, db, user, pk(c.typ, c.older, withTransfers))
			if err != nil {
				t.Fatal(err)
			}
			if got.snapshot.GetPeriodKey() != c.older || deref(got.previous) != c.olderInTheYear || deref(got.next) != c.newer {
				t.Fatalf("%s: snapshot %q navigation %s / %s, want %s / %s", c.older, got.snapshot.GetPeriodKey(),
					deref(got.previous), deref(got.next), c.olderInTheYear, c.newer)
			}
			got, err = readAnalytics(ctx, db, user, pk(c.typ, c.newer, withTransfers))
			if err != nil {
				t.Fatal(err)
			}
			if got.snapshot.GetPeriodKey() != c.newer || deref(got.previous) != c.older || got.next != nil {
				t.Fatalf("%s: snapshot %q navigation %s / %s, want %s / none (%s is removed)", c.newer, got.snapshot.GetPeriodKey(),
					deref(got.previous), deref(got.next), c.older, c.removedAfter)
			}

			periods, err := listPeriods(ctx, db, user, domain.PeriodType(c.typ))
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, p := range periods.Items {
				keys = append(keys, p.Key)
			}
			if want := []string{c.newer, c.older, c.olderInTheYear}; !slices.Equal(keys, want) {
				t.Fatalf("periods %v, want %v", keys, want)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

// Every expected query is served by the primary keys, without a scan of other users' rows or
// another index, and — where the order matters for the answer — without a sort. The planner is
// pushed off sequential scans because the test tables are tiny; what is checked is that an index
// path exists. The cascade of PurgeUserData runs, per header, the delete of its gaps and metrics
// by the full snapshot key; those statements stand in for it.
func TestIntegrationQueriesUsePrimaryKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	for i := range 3 {
		seedForReads(t, db, newUser(i))
	}
	// Ten years of months of the user, so that the planner prices a scan of all the user's
	// metrics above a lookup by the full snapshot key.
	if _, err := db.Exec(ctx, `
		WITH p AS (
			INSERT INTO period_snapshot (user_id, period_type, period_key, transfer_mode, calc_version, removed)
			SELECT $1, $2, to_char(date '2010-01-01' + make_interval(months => k), 'YYYY-MM'), m, 1, false
			FROM generate_series(0, 119) k, generate_series(1, 2) m
			RETURNING user_id, period_type, period_key, transfer_mode
		), g AS (
			INSERT INTO period_gap (user_id, period_type, period_key, transfer_mode, ordinal, gap_start, gap_end)
			SELECT p.*, 0, now(), now() FROM p
		)
		INSERT INTO period_metric (user_id, period_type, period_key, transfer_mode, metric_id, definition_version, status)
		SELECT p.*, 'm' || i, 1, 1 FROM p, generate_series(1, 15) i`, newUser(1), month); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `ANALYZE period_snapshot, period_gap, period_metric, user_data_state`); err != nil {
		t.Fatal(err)
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
	applied := []any{user, []int16{month, month}, []string{"2026-08", "2026-09"}, []int16{withTransfers, withTransfers}}
	cases := []struct {
		name  string
		sql   string
		args  []any
		index []string
		// sortOK: the only sort is ORDER BY metric_id over the metrics of one period (a few dozen
		// rows at most); the planner cannot carry the index order through the join with the head.
		sortOK bool
		// cond: parts of the plan that must be there at least so many times, such as an index
		// condition on the last column of the snapshot key (a lookup by the full key).
		cond map[string]int
	}{
		{"get analytics", readAnalyticsSQL, []any{user, month, "2026-08", withTransfers},
			[]string{"user_data_state_pkey", "period_snapshot_pkey", "period_gap_pkey", "period_metric_pkey"}, true,
			map[string]int{"(transfer_mode = p.transfer_mode)": 1, "(transfer_mode = '1'::smallint)": 2}},
		{"latest period", latestPeriodSQL, []any{user, month}, []string{"period_snapshot_pkey"}, false, nil},
		// sortOK: the outer ORDER BY of the lateral join sorts the periods of one user and type.
		{"list periods", listPeriodsSQL, []any{user, month}, []string{"user_data_state_pkey", "period_snapshot_pkey"}, true, nil},
		// The removal check reads the rows of the user by the key prefix (at most the few hundred
		// headers of one user), never the whole table.
		{"headers and markers", upsertHeadersSQL, []any{user, int32(2), []int16{month, month}, []string{"2026-08", "2026-08"},
			[]int16{withTransfers, withoutTransfers}, []bool{true, true}, []*time.Time{nil, nil}, []*time.Time{nil, nil},
			[]*int16{nil, nil}, []*time.Time{nil, nil}, []*time.Time{nil, nil}, []*int32{nil, nil}, []*time.Time{nil, nil}},
			[]string{"period_snapshot_pkey"}, true, nil},
		{"replace children", deleteChildrenSQL, applied, []string{"period_gap_pkey", "period_metric_pkey"}, false,
			map[string]int{"AND (transfer_mode = a": 2}},
		{"purge headers", `DELETE FROM period_snapshot WHERE user_id = $1`, []any{user}, []string{"period_snapshot_pkey"}, false, nil},
		{"purge cascade to gaps", `DELETE FROM period_gap WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
			[]any{user, month, "2026-08", withTransfers}, []string{"period_gap_pkey"}, false, nil},
		{"purge cascade to metrics", `DELETE FROM period_metric WHERE user_id = $1 AND period_type = $2 AND period_key = $3 AND transfer_mode = $4`,
			[]any{user, month, "2026-08", withTransfers}, []string{"period_metric_pkey"}, false, nil},
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
			for part, n := range c.cond {
				if strings.Count(plan, part) < n {
					t.Errorf("plan has %q fewer than %d times:\n%s", part, n, plan)
				}
			}
			for _, idx := range c.index {
				if !strings.Contains(plan, idx) {
					t.Errorf("plan does not use %s:\n%s", idx, plan)
				}
			}
			bad := []string{"Seq Scan"}
			if !c.sortOK {
				bad = append(bad, "Sort")
			}
			for _, b := range bad {
				if strings.Contains(plan, b) {
					t.Errorf("plan has %s:\n%s", b, plan)
				}
			}
		})
	}
}

// Every table of the schema and every column of it carries a description in Russian
// (COMMENT ON TABLE, COMMENT ON COLUMN, owner's request in the review of FIN-113), so a table
// or a column added later without one fails here. schema_migrations belongs to golang-migrate.
func TestIntegrationEveryTableAndColumnHasDescription(t *testing.T) {
	db := openDB(t)
	// One row per table (column NULL) and per column of it.
	rows, err := db.Query(context.Background(), `
		WITH described_table AS (
			SELECT c.oid, c.relname
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public'
			  AND c.relkind IN ('r', 'p')
			  AND NOT c.relispartition
			  AND c.relname <> 'schema_migrations'
		)
		SELECT relname, NULL::text, coalesce(obj_description(oid, 'pg_class'), '')
		FROM described_table
		UNION ALL
		SELECT t.relname, a.attname::text, coalesce(col_description(t.oid, a.attnum), '')
		FROM described_table t
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1, 2 NULLS FIRST`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		table       string
		column      *string
		description string
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object, error) {
		var o object
		err := row.Scan(&o.table, &o.column, &o.description)
		return o, err
	})
	if err != nil {
		t.Fatal(err)
	}
	var tables, columns int
	for _, o := range objects {
		name, statement := "table "+o.table, "COMMENT ON TABLE"
		if o.column != nil {
			name, statement = "column "+o.table+"."+*o.column, "COMMENT ON COLUMN"
			columns++
		} else {
			tables++
		}
		description := strings.TrimSpace(o.description)
		switch {
		case description == "":
			t.Errorf("%s has no %s", name, statement)
		case !startsInRussian.MatchString(description):
			t.Errorf("%s: description does not start in Russian: %q", name, description)
		}
	}
	if tables == 0 || columns == 0 {
		t.Fatalf("found %d tables and %d columns in schema public", tables, columns)
	}
}

// startsInRussian accepts a description whose first letter is Cyrillic.
var startsInRussian = regexp.MustCompile(`^\p{Cyrillic}`)
