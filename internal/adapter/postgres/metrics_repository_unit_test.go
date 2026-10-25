package postgres

// Unit tests of the repository's Go branching without PostgreSQL: which statements a request
// sends and in what order, what is skipped, how errors and rows are translated. The SQL itself
// is checked on PostgreSQL in metrics_schema_test.go.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/analytics/internal/domain"
)

// scriptedRows plays pgx.Rows over prepared values; Scan assigns them by reflection, a nil
// value leaves the destination at its zero (a NULL into a pointer).
type scriptedRows struct {
	values  [][]any
	i       int
	scanErr error
	err     error
}

func (r *scriptedRows) Close()                                       {}
func (r *scriptedRows) Err() error                                   { return r.err }
func (r *scriptedRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *scriptedRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *scriptedRows) Conn() *pgx.Conn                              { return nil }
func (r *scriptedRows) TypeMap() *pgtype.Map                         { return nil }
func (r *scriptedRows) RawValues() [][]byte                          { return nil }
func (r *scriptedRows) Values() ([]any, error)                       { return r.values[r.i-1], nil }
func (r *scriptedRows) Next() bool {
	if r.i >= len(r.values) {
		return false
	}
	r.i++
	return true
}

func (r *scriptedRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.values[r.i-1]
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if row[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(row[i]))
	}
	return nil
}

// scriptedTx is the transaction of a unit DB: it records statements and answers from a script.
type scriptedTx struct {
	pgx.Tx
	statements []string
	rows       map[string]*scriptedRows
	queryErr   map[string]error
	execErr    map[string]error
}

func statementName(sql string) string {
	for name, s := range map[string]string{
		"state": upsertStateSQL, "headers": upsertHeadersSQL, "children": deleteChildrenSQL,
		"gaps": insertGapsSQL, "metrics": insertMetricsSQL, "list": listPeriodsSQL, "purge": purgeSQL,
	} {
		if s == sql {
			return name
		}
	}
	return "unknown"
}

func (t *scriptedTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	name := statementName(sql)
	t.statements = append(t.statements, name)
	if err := t.queryErr[name]; err != nil {
		return nil, err
	}
	if r, ok := t.rows[name]; ok {
		return r, nil
	}
	return &scriptedRows{}, nil
}

func (t *scriptedTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	name := statementName(sql)
	t.statements = append(t.statements, name)
	return pgconn.CommandTag{}, t.execErr[name]
}

func (*scriptedTx) Commit(context.Context) error   { return nil }
func (*scriptedTx) Rollback(context.Context) error { return nil }

func unitRepository(tx *scriptedTx) (*Metrics, *DB) {
	db := &DB{transactions: pgstore.New(unitBeginner{tx})}
	return NewMetrics(db), db
}

func instant(day int) *time.Time {
	t := time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC)
	return &t
}

func unitSnapshot(key string, mode domain.TransferMode, metrics ...string) domain.Snapshot {
	s := domain.Snapshot{
		Key:   domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: key}, Mode: mode},
		Range: &domain.TimeRange{Start: instant(1), End: instant(30)}, Coverage: domain.CoveragePartial,
		DataFrom: instant(1), DataTo: instant(20), CalculatedAt: instant(21),
		Gaps: []domain.TimeRange{{Start: instant(3), End: instant(4)}},
	}
	for _, id := range metrics {
		s.Metrics = append(s.Metrics, domain.Metric{ID: id, Status: 1})
	}
	return s
}

func applied(key string, mode domain.TransferMode, removed bool) []any {
	return []any{int16(domain.PeriodMonth), key, int16(mode), removed}
}

func TestUnitRepositoryWritesOnlyInsideItsTransaction(t *testing.T) {
	repo, _ := unitRepository(&scriptedTx{})
	ctx := context.Background()
	if err := repo.UpsertState(ctx, "u", 1, domain.State{}); !errors.Is(err, ErrTransactionRequired) {
		t.Fatalf("state: %v", err)
	}
	if _, err := repo.UpsertPeriods(ctx, "u", 1, nil, nil); !errors.Is(err, ErrTransactionRequired) {
		t.Fatalf("periods: %v", err)
	}
	if err := repo.Purge(ctx, "u"); !errors.Is(err, ErrTransactionRequired) {
		t.Fatalf("purge: %v", err)
	}
}

func TestUnitUpsertPeriodsSendsStatementsOfAppliedKeysOnly(t *testing.T) {
	tx := &scriptedTx{rows: map[string]*scriptedRows{"headers": {values: [][]any{
		applied("2026-09", domain.TransferWith, false), applied("2026-07", domain.TransferWith, true), applied("2026-07", domain.TransferWithout, true),
	}}}}
	repo, db := unitRepository(tx)
	var n int
	err := db.Run(context.Background(), func(ctx context.Context) error {
		var err error
		// 2026-08 is skipped as stale: no children are written for it.
		n, err = repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{
			unitSnapshot("2026-09", domain.TransferWith, "tiles.expense"), unitSnapshot("2026-08", domain.TransferWith, "tiles.income"),
		}, []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2026-07"}})
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("applied %d, %v; want 1 (markers are not counted)", n, err)
	}
	if got := strings.Join(tx.statements, ","); got != "headers,children,gaps,metrics" {
		t.Fatalf("statements %s", got)
	}

	// Nothing applied: only the header statement. Nothing sent: no statement at all.
	tx.statements, tx.rows = nil, nil
	if err := db.Run(context.Background(), func(ctx context.Context) error {
		_, err := repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{unitSnapshot("2026-09", domain.TransferWith)}, nil)
		return err
	}); err != nil || strings.Join(tx.statements, ",") != "headers" {
		t.Fatalf("stale request: %v %v", tx.statements, err)
	}
	tx.statements = nil
	if err := db.Run(context.Background(), func(ctx context.Context) error {
		_, err := repo.UpsertPeriods(ctx, "u", 3, nil, nil)
		return err
	}); err != nil || len(tx.statements) != 0 {
		t.Fatalf("empty request: %v %v", tx.statements, err)
	}
	// Only markers applied, or a snapshot without metrics and gaps: no insert statements.
	tx.statements = nil
	tx.rows = map[string]*scriptedRows{"headers": {values: [][]any{applied("2026-09", domain.TransferWith, false)}}}
	bare := unitSnapshot("2026-09", domain.TransferWith)
	bare.Gaps = nil
	if err := db.Run(context.Background(), func(ctx context.Context) error {
		_, err := repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{bare}, nil)
		return err
	}); err != nil || strings.Join(tx.statements, ",") != "headers,children" {
		t.Fatalf("snapshot without children: %v %v", tx.statements, err)
	}
}

func TestUnitUpsertPeriodsStopsAtTheFirstError(t *testing.T) {
	boom := errors.New("boom")
	header := func() map[string]*scriptedRows {
		return map[string]*scriptedRows{"headers": {values: [][]any{applied("2026-09", domain.TransferWith, false)}}}
	}
	for name, tx := range map[string]*scriptedTx{
		"headers query": {queryErr: map[string]error{"headers": boom}},
		"headers scan":  {rows: map[string]*scriptedRows{"headers": {values: [][]any{applied("2026-09", domain.TransferWith, false)}, scanErr: boom}}},
		"headers rows":  {rows: map[string]*scriptedRows{"headers": {err: boom}}},
		"children":      {rows: header(), execErr: map[string]error{"children": boom}},
		"gaps":          {rows: header(), execErr: map[string]error{"gaps": boom}},
		"metrics":       {rows: header(), execErr: map[string]error{"metrics": boom}},
	} {
		t.Run(name, func(t *testing.T) {
			repo, db := unitRepository(tx)
			err := db.Run(context.Background(), func(ctx context.Context) error {
				_, err := repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{unitSnapshot("2026-09", domain.TransferWith, "tiles.expense")}, nil)
				return err
			})
			if !errors.Is(err, boom) {
				t.Fatalf("error %v, want %v", err, boom)
			}
		})
	}
	// A mapping refusal is returned before any statement is sent.
	for name, s := range map[string]domain.Snapshot{
		"header without range": {Key: domain.SnapshotKey{Mode: domain.TransferWith}},
		"enum out of range": func() domain.Snapshot {
			s := unitSnapshot("2026-09", domain.TransferWith)
			s.Coverage = 1 << 20
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			tx := &scriptedTx{}
			repo, db := unitRepository(tx)
			err := db.Run(context.Background(), func(ctx context.Context) error {
				_, err := repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{s}, nil)
				return err
			})
			if err == nil || len(tx.statements) != 0 {
				t.Fatalf("error %v after statements %v", err, tx.statements)
			}
		})
	}
	// Gaps and metrics the mapping refuses after the header was applied stop the request.
	for name, change := range map[string]func(*domain.Snapshot){
		"too many gaps":   func(s *domain.Snapshot) { s.Gaps = make([]domain.TimeRange, 1<<15+1) },
		"metric enum":     func(s *domain.Snapshot) { s.Metrics[0].Status = 1 << 20 },
		"lock reason":     func(s *domain.Snapshot) { s.Metrics[0].Lock = &domain.Lock{Reason: 1 << 20} },
		"filter kind":     func(s *domain.Snapshot) { s.Metrics[0].Filter = &domain.Filter{Kind: 1 << 20} },
		"filter transfer": func(s *domain.Snapshot) { s.Metrics[0].Filter = &domain.Filter{TransferMode: 1 << 20} },
	} {
		t.Run(name, func(t *testing.T) {
			tx := &scriptedTx{rows: map[string]*scriptedRows{"headers": {values: [][]any{applied("2026-09", domain.TransferWith, false)}}}}
			repo, db := unitRepository(tx)
			s := unitSnapshot("2026-09", domain.TransferWith, "tiles.expense")
			change(&s)
			err := db.Run(context.Background(), func(ctx context.Context) error {
				_, err := repo.UpsertPeriods(ctx, "u", 3, []domain.Snapshot{s}, nil)
				return err
			})
			if err == nil || strings.Contains(strings.Join(tx.statements, ","), "gaps,metrics") {
				t.Fatalf("error %v, statements %v", err, tx.statements)
			}
		})
	}
}

func TestUnitUpsertStateAndPurge(t *testing.T) {
	tx := &scriptedTx{rows: map[string]*scriptedRows{"state": {values: [][]any{{"u"}}}}}
	repo, db := unitRepository(tx)
	err := db.Run(context.Background(), func(ctx context.Context) error {
		if err := repo.UpsertState(ctx, "u", 2, domain.State{TimeZone: "UTC", CalculatedAt: instant(1)}); err != nil {
			return err
		}
		return repo.Purge(ctx, "u")
	})
	if err != nil || strings.Join(tx.statements, ",") != "state,purge" {
		t.Fatalf("statements %v, %v", tx.statements, err)
	}
	boom := errors.New("boom")
	for name, tx := range map[string]*scriptedTx{
		"state": {queryErr: map[string]error{"state": boom}},
		"purge": {execErr: map[string]error{"purge": boom}},
	} {
		repo, db := unitRepository(tx)
		err := db.Run(context.Background(), func(ctx context.Context) error {
			if name == "state" {
				return repo.UpsertState(ctx, "u", 2, domain.State{})
			}
			return repo.Purge(ctx, "u")
		})
		if !errors.Is(err, boom) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestUnitListPeriodsTranslatesRows(t *testing.T) {
	zone, start, end := "Europe/Moscow", *instant(1), *instant(30)
	key, coverage := "2026-09", int16(1)
	cases := map[string]struct {
		rows    *scriptedRows
		want    domain.Periods
		wantErr error
	}{
		"periods": {rows: &scriptedRows{values: [][]any{{&zone, &key, &start, &end, &coverage}}},
			want: domain.Periods{TimeZone: zone, Items: []domain.PeriodItem{{Key: key, Start: start, End: end, Coverage: domain.CoverageComplete}}}},
		"neither state nor periods": {rows: &scriptedRows{values: [][]any{{nil, nil, nil, nil, nil}}}},
		"a state without periods":   {rows: &scriptedRows{values: [][]any{{&zone, nil, nil, nil, nil}}}, want: domain.Periods{TimeZone: zone}},
		"a header without its data": {rows: &scriptedRows{values: [][]any{{&zone, &key, nil, &end, &coverage}}}, wantErr: errBrokenHeader},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tx := &scriptedTx{rows: map[string]*scriptedRows{"list": c.rows}}
			got, err := listPeriods(context.Background(), tx, "u", domain.PeriodMonth)
			if !errors.Is(err, c.wantErr) || !reflect.DeepEqual(got, c.want) && c.wantErr == nil {
				t.Fatalf("periods %+v, %v; want %+v, %v", got, err, c.want, c.wantErr)
			}
		})
	}
	boom := errors.New("boom")
	for name, tx := range map[string]*scriptedTx{
		"query": {queryErr: map[string]error{"list": boom}},
		"scan":  {rows: map[string]*scriptedRows{"list": {values: [][]any{{nil}}, scanErr: boom}}},
		"rows":  {rows: map[string]*scriptedRows{"list": {err: boom}}},
	} {
		if _, err := listPeriods(context.Background(), tx, "u", domain.PeriodMonth); !errors.Is(err, boom) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The repository reads through the transaction when there is one.
	tx := &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{{&zone, nil, nil, nil, nil}}}}}
	repo, db := unitRepository(tx)
	if err := db.Run(context.Background(), func(ctx context.Context) error {
		p, err := repo.ListPeriods(ctx, "u", domain.PeriodMonth)
		if err == nil && p.TimeZone != zone {
			err = errors.New("zone lost")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
