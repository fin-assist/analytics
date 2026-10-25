package metrics

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

const user = "01a107db-ae80-7b07-b8a1-000000000001"

// fakeTx runs fn and reports the outcome like postgres.DB.Run: the error of fn is the error
// of the transaction, and nothing of it is committed then.
type fakeTx struct {
	committed, rolledBack int
	err                   error
}

func (f *fakeTx) Run(ctx context.Context, fn func(context.Context) error) error {
	if f.err != nil {
		return f.err
	}
	if err := fn(ctx); err != nil {
		f.rolledBack++
		return err
	}
	f.committed++
	return nil
}

type fakeGuard struct {
	deleted bool
	err     error
	calls   []string
}

func (f *fakeGuard) Guard(_ context.Context, id string) (bool, error) {
	f.calls = append(f.calls, id)
	return !f.deleted, f.err
}

type fakeStore struct {
	order    []string
	applied  int
	stateErr error
	err      error
	periods  domain.Periods
}

func (f *fakeStore) UpsertState(context.Context, string, int32, domain.State) error {
	f.order = append(f.order, "state")
	return f.stateErr
}

func (f *fakeStore) UpsertPeriods(_ context.Context, _ string, _ int32, snapshots []domain.Snapshot, removed []domain.PeriodKey) (int, error) {
	f.order = append(f.order, "periods")
	return f.applied, f.err
}

func (f *fakeStore) ListPeriods(_ context.Context, id string, typ domain.PeriodType) (domain.Periods, error) {
	f.order = append(f.order, "list "+id+" "+typ.String())
	return f.periods, f.err
}

type fakeObserver struct{ periods []domain.PeriodKey }

func (f *fakeObserver) IncompletePeriod(_ context.Context, p domain.PeriodKey) {
	f.periods = append(f.periods, p)
}

func moment(day int) *time.Time {
	t := time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC)
	return &t
}

func snapshot(key string, mode domain.TransferMode) domain.Snapshot {
	return domain.Snapshot{
		Key:   domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: key}, Mode: mode},
		Range: &domain.TimeRange{Start: moment(1), End: moment(30)}, Coverage: domain.CoveragePartial,
		DataFrom: moment(1), DataTo: moment(25), CalculatedAt: moment(26),
	}
}

func batch() domain.Upsert {
	return domain.Upsert{UserID: user, Version: 3,
		State: &domain.State{TimeZone: "UTC", CalculatedAt: moment(26)},
		Snapshots: []domain.Snapshot{
			snapshot("2026-09", domain.TransferWith), snapshot("2026-09", domain.TransferWithout),
			snapshot("2026-08", domain.TransferWith),
		},
		Removed: []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2026-07"}},
	}
}

func setup(t *testing.T) (*Metrics, *fakeTx, *fakeGuard, *fakeStore, *fakeObserver) {
	t.Helper()
	tx, guard, store, observer := &fakeTx{}, &fakeGuard{}, &fakeStore{applied: 2}, &fakeObserver{}
	m, err := New(tx, store, guard, observer)
	if err != nil {
		t.Fatal(err)
	}
	return m, tx, guard, store, observer
}

func TestUnitNewRequiresDependencies(t *testing.T) {
	if _, err := New(nil, &fakeStore{}, &fakeGuard{}, &fakeObserver{}); err == nil {
		t.Fatal("nil transactions accepted")
	}
	if _, err := New(&fakeTx{}, nil, &fakeGuard{}, &fakeObserver{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := New(&fakeTx{}, &fakeStore{}, nil, &fakeObserver{}); err == nil {
		t.Fatal("nil guard accepted")
	}
	if _, err := New(&fakeTx{}, &fakeStore{}, &fakeGuard{}, nil); err == nil {
		t.Fatal("nil observer accepted")
	}
}

// The main path: guard, state, periods in one transaction; counts of applied and skipped
// snapshots; the one-mode period reported after the commit.
func TestUnitUpsertAppliesInOrder(t *testing.T) {
	m, tx, guard, store, observer := setup(t)
	got, err := m.Upsert(t.Context(), batch())
	if err != nil {
		t.Fatal(err)
	}
	if got != (Result{Applied: 2, Skipped: 1}) || tx.committed != 1 {
		t.Fatalf("result %+v, %d commits", got, tx.committed)
	}
	if !slices.Equal(guard.calls, []string{user}) || !slices.Equal(store.order, []string{"state", "periods"}) {
		t.Fatalf("guard %v, store %v", guard.calls, store.order)
	}
	if want := []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2026-08"}}; !slices.Equal(observer.periods, want) {
		t.Fatalf("incomplete %v, want %v", observer.periods, want)
	}
}

func TestUnitUpsertWithoutStateSkipsIt(t *testing.T) {
	m, _, _, store, _ := setup(t)
	u := batch()
	u.State = nil
	if _, err := m.Upsert(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.order, []string{"periods"}) {
		t.Fatalf("store %v", store.order)
	}
}

// A broken batch writes nothing: no transaction, no guard.
func TestUnitUpsertRefusesInvalidBatchBeforeTransaction(t *testing.T) {
	m, tx, guard, store, observer := setup(t)
	u := batch()
	u.Snapshots[0].Metrics = []domain.Metric{{ID: "tiles.expense", Status: domain.StatusUnavailable}}
	if _, err := m.Upsert(t.Context(), u); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error %v, want ErrInvalid", err)
	}
	if tx.committed+tx.rolledBack != 0 || len(guard.calls) != 0 || len(store.order) != 0 || len(observer.periods) != 0 {
		t.Fatal("a refused batch reached the transaction")
	}
}

// A deleted user's late batch is acknowledged without effects: nothing written, nothing reported.
func TestUnitUpsertForDeletedUserWritesNothing(t *testing.T) {
	m, tx, guard, store, observer := setup(t)
	guard.deleted = true
	got, err := m.Upsert(t.Context(), batch())
	if err != nil || got != (Result{}) {
		t.Fatalf("result %+v, %v; want zero and nil", got, err)
	}
	if len(store.order) != 0 || len(observer.periods) != 0 || tx.committed != 1 {
		t.Fatalf("store %v, observer %v", store.order, observer.periods)
	}
}

func TestUnitUpsertRollsBackOnFailure(t *testing.T) {
	boom := errors.New("boom")
	for name, change := range map[string]func(*fakeTx, *fakeGuard, *fakeStore){
		"guard":       func(_ *fakeTx, g *fakeGuard, _ *fakeStore) { g.err = boom },
		"state":       func(_ *fakeTx, _ *fakeGuard, s *fakeStore) { s.stateErr = boom },
		"periods":     func(_ *fakeTx, _ *fakeGuard, s *fakeStore) { s.err = boom },
		"transaction": func(tx *fakeTx, _ *fakeGuard, _ *fakeStore) { tx.err = boom },
	} {
		t.Run(name, func(t *testing.T) {
			m, tx, guard, store, observer := setup(t)
			change(tx, guard, store)
			if _, err := m.Upsert(t.Context(), batch()); !errors.Is(err, boom) {
				t.Fatalf("error %v, want %v", err, boom)
			}
			if tx.committed != 0 || len(observer.periods) != 0 {
				t.Fatal("a failed batch was committed or reported")
			}
		})
	}
}

func TestUnitPeriods(t *testing.T) {
	m, _, _, store, _ := setup(t)
	store.periods = domain.Periods{TimeZone: "UTC", Items: []domain.PeriodItem{{Key: "2026-09"}}}
	got, err := m.Periods(t.Context(), user, domain.PeriodQuarter)
	if err != nil || got.TimeZone != "UTC" || len(got.Items) != 1 {
		t.Fatalf("periods %+v, %v", got, err)
	}
	if !slices.Equal(store.order, []string{"list " + user + " quarter"}) {
		t.Fatalf("store %v", store.order)
	}
	if _, err := m.Periods(t.Context(), "u1", domain.PeriodMonth); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid user: %v", err)
	}
	if _, err := m.Periods(t.Context(), user, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown type: %v", err)
	}
	boom := errors.New("boom")
	store.err = boom
	if _, err := m.Periods(t.Context(), user, domain.PeriodMonth); !errors.Is(err, boom) {
		t.Fatalf("store error: %v", err)
	}
}
