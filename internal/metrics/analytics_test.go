package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

type screenStore struct {
	data  domain.AnalyticsData
	err   error
	key   *string
	calls int
}

func (f *screenStore) ReadAnalytics(_ context.Context, _ string, _ domain.PeriodType, key *string, _ domain.TransferMode) (domain.AnalyticsData, error) {
	f.key = key
	f.calls++
	return f.data, f.err
}

func TestUnitScreenCalendarAndCurrentState(t *testing.T) {
	zone, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		typ       domain.PeriodType
		key, want string
		months    int
	}{
		{domain.PeriodMonth, "2026-03", "2026-03", 1}, {domain.PeriodQuarter, "2026-03", "2026-Q1", 3}, {domain.PeriodQuarter, "2026-Q4", "2026-Q4", 3}, {domain.PeriodYear, "2026-03", "2026", 12}, {domain.PeriodYear, "2026-Q2", "2026", 12}, {domain.PeriodYear, "2026", "2026", 12},
	} {
		t.Run(tc.key+tc.typ.String(), func(t *testing.T) {
			from := time.Date(2025, 1, 1, 0, 0, 0, 0, zone)
			to := time.Date(2026, 4, 1, 0, 0, 0, 0, zone)
			stale := to.AddDate(0, 0, 14)
			store := &screenStore{data: domain.AnalyticsData{State: &domain.State{HasData: true, DataFrom: &from, DataTo: &to, LastOperationAt: &to, StaleAfter: &stale, TimeZone: zone.String(), CalculatedAt: &to}}}
			now := time.Date(2026, 3, 20, 12, 0, 0, 0, zone)
			uc, e := NewScreen(store, func() time.Time { return now })
			if e != nil {
				t.Fatal(e)
			}
			got, e := uc.Get(t.Context(), "01a107db-ae80-7b07-b8a1-000000000001", tc.typ, &tc.key, domain.TransferWithout)
			if e != nil {
				t.Fatal(e)
			}
			if *store.key != tc.want || got.Key.Key != tc.want || got.Key.Mode != domain.TransferWithout || got.Stale || got.Range.Start.Location().String() != zone.String() {
				t.Fatalf("screen %+v", got)
			}
			if !got.Range.Start.AddDate(0, tc.months, 0).Equal(*got.Range.End) {
				t.Fatal("calendar end")
			}
			now = *got.Range.Start
			got, e = uc.Get(t.Context(), "01a107db-ae80-7b07-b8a1-000000000001", tc.typ, &tc.key, domain.TransferWith)
			if e != nil || !got.Current {
				t.Fatalf("start inclusion %+v %v", got, e)
			}
			now = *got.Range.End
			got, e = uc.Get(t.Context(), "01a107db-ae80-7b07-b8a1-000000000001", tc.typ, &tc.key, domain.TransferWith)
			if e != nil || got.Current {
				t.Fatalf("end exclusion %+v %v", got, e)
			}
			now = stale
			got, e = uc.Get(t.Context(), "01a107db-ae80-7b07-b8a1-000000000001", tc.typ, &tc.key, domain.TransferWith)
			if e != nil || !got.Stale {
				t.Fatalf("stale equality %+v %v", got, e)
			}
		})
	}
}

func TestUnitScreenValidationAndStoreErrors(t *testing.T) {
	f := &screenStore{}
	uc, e := NewScreen(f, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := NewScreen(nil, time.Now); e == nil {
		t.Fatal("nil store")
	}
	if _, e := NewScreen(f, nil); e == nil {
		t.Fatal("nil clock")
	}
	for _, tc := range []struct {
		user string
		typ  domain.PeriodType
		key  *string
		mode domain.TransferMode
	}{
		{"bad", domain.PeriodMonth, nil, domain.TransferWith}, {user, 9, nil, domain.TransferWith}, {user, domain.PeriodMonth, nil, 9},
		{user, domain.PeriodMonth, ptrString("2026"), domain.TransferWith}, {user, domain.PeriodMonth, ptrString("2026-Q1"), domain.TransferWith}, {user, domain.PeriodMonth, ptrString("0000-01"), domain.TransferWith}, {user, domain.PeriodYear, ptrString("9999"), domain.TransferWith}, {user, domain.PeriodMonth, ptrString("2026-99"), domain.TransferWith}, {user, domain.PeriodQuarter, ptrString("2026-Q5"), domain.TransferWith}, {user, domain.PeriodMonth, ptrString(""), domain.TransferWith},
	} {
		if _, e := uc.Get(t.Context(), tc.user, tc.typ, tc.key, tc.mode); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("invalid %+v: %v", tc, e)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid requests reached storage")
	}
	f.err = context.Canceled
	if _, e := uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	f.err = nil
	if got, e := uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith); e != nil || got.State != nil {
		t.Fatalf("absent %+v %v", got, e)
	}
	f.data.State = &domain.State{HasData: false}
	if _, e := uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith); e != nil {
		t.Fatal(e)
	}
	f.data.State.HasData = true
	if _, e := uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith); e == nil || errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("corrupt state must be internal: %v", e)
	}
}

func TestUnitScreenDefaultAndOldSnapshotRange(t *testing.T) {
	from, to, stale := *moment(1), *moment(20), *moment(30)
	key := "2026-09"
	store := &screenStore{data: domain.AnalyticsData{SelectedKey: &key, State: &domain.State{HasData: true, DataFrom: &from, DataTo: &to, StaleAfter: &stale, LastOperationAt: &to, TimeZone: "UTC", CalculatedAt: &to}}}
	uc, _ := NewScreen(store, func() time.Time { return to })
	got, e := uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith)
	if e != nil || got.Key.Key != key || store.key != nil {
		t.Fatalf("default %+v %v", got, e)
	}
	store.data.Snapshot = &domain.Snapshot{Range: &domain.TimeRange{Start: &from, End: &to}}
	got, e = uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith)
	if e != nil || got.Current || !got.Range.End.Equal(to) {
		t.Fatalf("old range %+v %v", got, e)
	}
	store.data.SelectedKey = nil
	if _, e = uc.Get(t.Context(), user, domain.PeriodMonth, nil, domain.TransferWith); e == nil {
		t.Fatal("inconsistent state accepted")
	}
}

func ptrString(s string) *string { return &s }

func TestUnitForecastHistoryWaitsForDailyAtProfileMonthBoundary(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	to := from.AddDate(0, 1, 0)
	stale := to.AddDate(0, 0, 14)
	calculated := to.Add(-time.Hour)
	key := "2026"
	history := &domain.ForecastHistory{}
	store := &screenStore{data: domain.AnalyticsData{History: history, State: &domain.State{HasData: true, DataFrom: &from, DataTo: &to, LastOperationAt: &calculated, StaleAfter: &stale, TimeZone: zone.String(), CalculatedAt: &calculated}}}
	now := to.Add(-time.Second).UTC()
	uc, err := NewScreen(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	read := func() *domain.ForecastHistory {
		t.Helper()
		got, err := uc.Get(t.Context(), user, domain.PeriodYear, &key, domain.TransferWith)
		if err != nil {
			t.Fatal(err)
		}
		return got.History
	}
	if read() != history {
		t.Fatal("UTC next day suppressed the still-current profile month")
	}
	now = to.UTC()
	if read() != nil {
		t.Fatal("previous month calculation explained today's month as unfinished")
	}
	store.data.State.CalculatedAt = &now
	if read() != history {
		t.Fatal("daily completion did not restore the detail")
	}
}

func TestUnitExplicitNoDataPeriodHasNoNavigationNeighbours(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	stale := to.AddDate(0, 0, 14)
	store := &screenStore{data: domain.AnalyticsData{Previous: ptrString("2026-08"), Next: ptrString("2026-10"), State: &domain.State{HasData: true, DataFrom: &from, DataTo: &to, LastOperationAt: &from, StaleAfter: &stale, TimeZone: "UTC", CalculatedAt: &from}}}
	uc, err := NewScreen(store, func() time.Time { return from })
	if err != nil {
		t.Fatal(err)
	}
	key := "2026-09"
	got, err := uc.Get(t.Context(), user, domain.PeriodMonth, &key, domain.TransferWith)
	if err != nil || got.Previous != nil || got.Next != nil {
		t.Fatalf("no-data neighbours %+v %v", got, err)
	}
}
