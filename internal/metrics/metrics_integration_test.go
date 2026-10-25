package metrics_test

// Integration tests of the usecase on PostgreSQL 18 with the production repository and the
// tombstone library: the order of statements in one transaction, the guard that serializes
// a user's writes against each other and against PurgeUserData, and that a deleted user's
// late batch writes nothing.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/go-migrate"
	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
	"github.com/fin-assist/analytics/internal/testdb"
	"github.com/fin-assist/analytics/migrations"
)

type env struct {
	db     *postgres.DB
	repo   *postgres.Metrics
	stones *tombstone.Store
	uc     *metrics.Metrics
	seen   *observed
}

type observed struct {
	mu      sync.Mutex
	periods []domain.PeriodKey
}

func (o *observed) IncompletePeriod(_ context.Context, p domain.PeriodKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.periods = append(o.periods, p)
}

func start(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(ctx, cfg, []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: dsn, MaxConns: 20, AppName: "metrics-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	stones, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewMetrics(db)
	seen := &observed{}
	uc, err := metrics.New(db, repo, stones, seen)
	if err != nil {
		t.Fatal(err)
	}
	return &env{db: db, repo: repo, stones: stones, uc: uc, seen: seen}
}

func userID(i int) string { return fmt.Sprintf("01a107db-ae80-7b07-b8a1-%012d", i) }

var msk = time.FixedZone("MSK", 3*3600)

func moment(t time.Time) *time.Time { return &t }

func monthSnapshot(key string, mode domain.TransferMode, version int32, metricIDs ...string) domain.Snapshot {
	start, err := time.ParseInLocation("2006-01", key, msk)
	if err != nil {
		panic(err)
	}
	s := domain.Snapshot{
		Key:   domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: key}, Mode: mode},
		Range: &domain.TimeRange{Start: moment(start), End: moment(start.AddDate(0, 1, 0))}, Coverage: domain.CoveragePartial,
		DataFrom: moment(start), DataTo: moment(start.Add(18 * time.Hour)), UnreadLines: version,
		CalculatedAt: moment(time.Date(2026, 9, 25, 21, 52, 0, 0, msk)),
	}
	for _, id := range metricIDs {
		v := int64(version)
		s.Metrics = append(s.Metrics, domain.Metric{ID: id, DefinitionVersion: 1, Status: 1, ValueMinor: &v})
	}
	return s
}

func state(version int32) *domain.State {
	from, to := time.Date(2026, 9, 1, 0, 0, 0, 0, msk), time.Date(2026, 9, 26, 0, 0, 0, 0, msk)
	return &domain.State{HasData: true, DataFrom: &from, DataTo: &to, LastOperationAt: moment(to.Add(-time.Hour)),
		StaleAfter: moment(to.AddDate(0, 0, 14)), FullMonths: version, TimeZone: "Europe/Moscow",
		CalculatedAt: moment(time.Date(2026, 9, 25, 21, 52, 0, 0, msk))}
}

func rows(t *testing.T, e *env, user string) (headers, metricRows int, version int32) {
	t.Helper()
	ctx := context.Background()
	if err := e.db.Primary().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM period_snapshot WHERE user_id = $1),
		(SELECT count(*) FROM period_metric WHERE user_id = $1),
		coalesce((SELECT calc_version FROM user_data_state WHERE user_id = $1), 0)`, user).Scan(&headers, &metricRows, &version); err != nil {
		t.Fatal(err)
	}
	return headers, metricRows, version
}

func TestIntegrationUpsertAndListThroughTheUsecase(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	user := userID(1)
	got, err := e.uc.Upsert(ctx, domain.Upsert{UserID: user, Version: 2, State: state(2), Snapshots: []domain.Snapshot{
		monthSnapshot("2026-09", domain.TransferWith, 2, "tiles.expense"), monthSnapshot("2026-09", domain.TransferWithout, 2, "tiles.expense"),
		monthSnapshot("2026-08", domain.TransferWith, 2, "tiles.income"),
	}, Removed: []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2026-07"}}})
	if err != nil || got != (metrics.Result{Applied: 3}) {
		t.Fatalf("first batch %+v, %v", got, err)
	}
	// Older and newer in one batch shape: the v1 redelivery is skipped.
	got, err = e.uc.Upsert(ctx, domain.Upsert{UserID: user, Version: 1, Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 1)}})
	if err != nil || got != (metrics.Result{Skipped: 1}) {
		t.Fatalf("stale batch %+v, %v", got, err)
	}
	periods, err := e.uc.Periods(ctx, user, domain.PeriodMonth)
	if err != nil || periods.TimeZone != "Europe/Moscow" || len(periods.Items) != 2 || periods.Items[0].Key != "2026-09" || periods.Items[1].Key != "2026-08" {
		t.Fatalf("periods %+v, %v", periods, err)
	}
	// 2026-08 of the first batch and 2026-09 of the stale one came in one mode only: each is
	// reported after the commit of its batch, applied or not.
	if len(e.seen.periods) != 2 || e.seen.periods[0].Key != "2026-08" || e.seen.periods[1].Key != "2026-09" {
		t.Fatalf("incomplete periods %v", e.seen.periods)
	}
	// A batch that fails in the middle (a gap without its end passes neither validation nor
	// NOT NULL, here it reaches the database through the repository) leaves nothing behind.
	bad := monthSnapshot("2026-06", domain.TransferWith, 3)
	bad.Gaps = []domain.TimeRange{{Start: bad.DataFrom}}
	err = e.db.Run(ctx, func(ctx context.Context) error {
		if err := e.repo.UpsertState(ctx, user, 3, *state(3)); err != nil {
			return err
		}
		_, err := e.repo.UpsertPeriods(ctx, user, 3, []domain.Snapshot{bad}, nil)
		return err
	})
	if err == nil {
		t.Fatal("a gap without its end was written")
	}
	if _, _, version := rows(t, e, user); version != 2 {
		t.Fatalf("state is v%d after a rolled back batch, want v2", version)
	}
}

// The repository refuses to write in autocommit: the guard and the statements must commit together.
func TestIntegrationRepositoryWritesOnlyInsideTheTransaction(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	if err := e.repo.UpsertState(ctx, userID(1), 1, *state(1)); !errors.Is(err, postgres.ErrTransactionRequired) {
		t.Fatalf("state outside Run: %v", err)
	}
	if _, err := e.repo.UpsertPeriods(ctx, userID(1), 1, nil, nil); !errors.Is(err, postgres.ErrTransactionRequired) {
		t.Fatalf("periods outside Run: %v", err)
	}
	if err := e.repo.Purge(ctx, userID(1)); !errors.Is(err, postgres.ErrTransactionRequired) {
		t.Fatalf("purge outside Run: %v", err)
	}
	if headers, _, version := rows(t, e, userID(1)); headers != 0 || version != 0 {
		t.Fatal("a write outside the transaction reached the database")
	}
}

// waitBlocked waits, with a deadline, until a backend of the test waits on an advisory lock:
// the tombstone guard of the user is taken by another transaction.
func waitBlocked(t *testing.T, e *env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := e.db.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("the batch did not wait for the tombstone guard")
		case <-tick.C:
		}
	}
}

// PurgeUserData that holds the guard first makes a concurrent batch wait, then see the
// tombstone and write nothing: no data comes back after the deletion.
func TestIntegrationBatchAfterConcurrentPurgeWritesNothing(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	user := userID(7)
	if _, err := e.uc.Upsert(ctx, domain.Upsert{UserID: user, Version: 1, State: state(1),
		Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 1, "tiles.expense")}}); err != nil {
		t.Fatal(err)
	}
	purged := make(chan struct{})
	release := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		purgeDone <- e.db.Run(ctx, func(ctx context.Context) error {
			if err := e.stones.Purge(ctx, user, e.repo.Purge); err != nil {
				return err
			}
			close(purged)
			<-release
			return nil
		})
	}()
	<-purged
	batch := make(chan error, 1)
	var result metrics.Result
	go func() {
		var err error
		result, err = e.uc.Upsert(ctx, domain.Upsert{UserID: user, Version: 2, State: state(2),
			Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 2, "tiles.expense")}})
		batch <- err
	}()
	waitBlocked(t, e)
	close(release)
	if err := <-purgeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-batch; err != nil || result != (metrics.Result{}) {
		t.Fatalf("late batch %+v, %v; want acknowledged without effects", result, err)
	}
	if headers, metricRows, version := rows(t, e, user); headers+metricRows != 0 || version != 0 {
		t.Fatalf("after purge and a late batch: %d headers, %d metrics, state v%d; want nothing", headers, metricRows, version)
	}
	// The repeated deletion is OK and the next batch is still acknowledged without effects.
	if err := e.db.Run(ctx, func(ctx context.Context) error { return e.stones.Purge(ctx, user, e.repo.Purge) }); err != nil {
		t.Fatal(err)
	}
	if got, err := e.uc.Upsert(ctx, domain.Upsert{UserID: user, Version: 3, State: state(3)}); err != nil || got != (metrics.Result{}) {
		t.Fatalf("batch after a repeated purge: %+v, %v", got, err)
	}
}

// One user in two cases of the UUID: the deletion of "01a1…" holds the tombstone guard, the
// batch of "01A1…" waits for it, sees the tombstone and writes nothing. go-tombstone builds the
// lock from the canonical lower-case UUID (uuid.Parse(id).String()) and tombstone_users.user_id
// is uuid, so the case of either call cannot split the lock or the lookup.
func TestIntegrationPurgeSerializesBatchesInAnotherCase(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	lower := userID(10)
	upper := strings.ToUpper(lower)
	purged := make(chan struct{})
	release := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		purgeDone <- e.db.Run(ctx, func(ctx context.Context) error {
			if err := e.stones.Purge(ctx, lower, e.repo.Purge); err != nil {
				return err
			}
			close(purged)
			<-release
			return nil
		})
	}()
	<-purged
	batch := make(chan error, 1)
	var result metrics.Result
	go func() {
		var err error
		result, err = e.uc.Upsert(ctx, domain.Upsert{UserID: upper, Version: 1, State: state(1),
			Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 1, "tiles.expense")}})
		batch <- err
	}()
	waitBlocked(t, e)
	close(release)
	if err := <-purgeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-batch; err != nil || result != (metrics.Result{}) {
		t.Fatalf("batch in another case: %+v, %v; want acknowledged without effects", result, err)
	}
	if headers, metricRows, version := rows(t, e, lower); headers+metricRows != 0 || version != 0 {
		t.Fatalf("after purge: %d headers, %d metrics, state v%d; want nothing", headers, metricRows, version)
	}
}

// A batch waits for a deletion that holds the guard first; a deletion waits for a batch that
// holds it first and then removes what the batch wrote.
func TestIntegrationPurgeAfterConcurrentBatchRemovesItsData(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	user := userID(8)
	other := userID(9)
	if _, err := e.uc.Upsert(ctx, domain.Upsert{UserID: other, Version: 1, State: state(1),
		Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 1, "tiles.expense")}}); err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.Primary().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The batch's transaction, played by hand: the guard taken, rows written, not committed.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 110))`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_data_state (user_id, calc_version, has_data, full_months, recalculating, time_zone, calculated_at)
		VALUES ($1, 1, false, 0, false, 'UTC', now())`, user); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- e.db.Run(ctx, func(ctx context.Context) error { return e.stones.Purge(ctx, user, e.repo.Purge) })
	}()
	waitBlocked(t, e)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if headers, _, version := rows(t, e, user); headers != 0 || version != 0 {
		t.Fatal("the purge that waited for a batch left its data")
	}
	if headers, _, version := rows(t, e, other); headers != 1 || version != 1 {
		t.Fatal("the purge removed another user's data")
	}
}

// Concurrent and out-of-order batches of one user through the usecase: the guard serializes
// them, so there is no deadlock and no error whatever the interleaving; every period sent in
// both modes ends at the highest version, a removal is applied to both modes or to none.
func TestIntegrationConcurrentBatchesThroughTheGuard(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	const writers = 8
	for round := range 5 {
		user := userID(100 + round)
		start := make(chan struct{})
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for _, v := range rand.Perm(writers) {
			version := int32(v + 1)
			u := domain.Upsert{UserID: user, Version: version}
			if version%2 == 0 {
				u.State = state(version)
			}
			for _, key := range []string{"2026-08", "2026-09"} {
				u.Snapshots = append(u.Snapshots, monthSnapshot(key, domain.TransferWith, version, "tiles.expense"),
					monthSnapshot(key, domain.TransferWithout, version, "tiles.expense"))
			}
			// 2026-07: removed by every third version, otherwise sent in one mode only.
			if version%3 == 0 {
				u.Removed = append(u.Removed, domain.PeriodKey{Type: domain.PeriodMonth, Key: "2026-07"})
			} else {
				u.Snapshots = append(u.Snapshots, monthSnapshot("2026-07", domain.TransferMode(1+version%2), version, "tiles.income"))
			}
			rand.Shuffle(len(u.Snapshots), func(i, j int) { u.Snapshots[i], u.Snapshots[j] = u.Snapshots[j], u.Snapshots[i] })
			wg.Go(func() {
				<-start
				_, err := e.uc.Upsert(ctx, u)
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		var versions []int32
		r, err := e.db.Primary().Query(ctx, `SELECT calc_version FROM period_snapshot
			WHERE user_id = $1 AND period_key IN ('2026-08', '2026-09') ORDER BY 1`, user)
		if err != nil {
			t.Fatal(err)
		}
		if versions, err = pgx.CollectRows(r, pgx.RowTo[int32]); err != nil {
			t.Fatal(err)
		}
		if len(versions) != 4 || versions[0] != writers || versions[3] != writers {
			t.Fatalf("round %d: versions %v, want four rows of v%d", round, versions, writers)
		}
		// Markers of 2026-07 come in pairs of one version unless a newer one-mode snapshot replaced one of them.
		r, err = e.db.Primary().Query(ctx, `SELECT transfer_mode, calc_version, removed FROM period_snapshot
			WHERE user_id = $1 AND period_key = '2026-07' ORDER BY transfer_mode`, user)
		if err != nil {
			t.Fatal(err)
		}
		type row struct {
			Mode    int16
			Version int32
			Removed bool
		}
		jul, err := pgx.CollectRows(r, pgx.RowToStructByPos[row])
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range jul {
			for j, b := range jul {
				if i != j && a.Removed && b.Version < a.Version {
					t.Fatalf("round %d: a v%d removal applied to one mode only next to v%d: %+v", round, a.Version, b.Version, jul)
				}
			}
		}
	}
}
