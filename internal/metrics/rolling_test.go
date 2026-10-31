package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitRollingScreenUsesCoverageCalendarAnchorAndExactSingleton(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Berlin")
	for _, tc := range []struct {
		typ        domain.PeriodType
		end, start string
	}{
		{5, "2026-10-31", "2025-10-31"}, {6, "2026-10-06", "2026-07-06"},
		{6, "2026-09-16", "2026-06-16"}, {6, "2024-05-31", "2024-02-29"},
		{6, "2023-05-31", "2023-02-28"}, {5, "2024-02-29", "2023-02-28"},
	} {
		t.Run(tc.end+tc.typ.String(), func(t *testing.T) {
			start, _ := time.ParseInLocation("2006-01-02", tc.start, zone)
			end, _ := time.ParseInLocation("2006-01-02", tc.end, zone)
			first := end.AddDate(-5, 0, 0)
			operation := end.AddDate(0, -1, 0)
			stale := end.AddDate(0, 0, 14)
			store := &screenStore{data: domain.AnalyticsData{Previous: ptrString("bad"), Next: ptrString("bad"), State: &domain.State{HasData: true, DataFrom: &first, DataTo: &end, LastOperationAt: &operation, StaleAfter: &stale, TimeZone: zone.String(), CalculatedAt: &operation}}}
			uc, _ := NewScreen(store, func() time.Time { return end.AddDate(1, 0, 0) })
			got, err := uc.Get(t.Context(), user, tc.typ, nil, domain.TransferWithout)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Range.Start.Equal(start) || !got.Range.End.Equal(end) || got.Current || got.Previous != nil || got.Next != nil || got.Key.Key != tc.typ.String() || store.key == nil || *store.key != tc.typ.String() {
				t.Fatalf("rolling screen %+v", got)
			}
			if got.Key.Mode != domain.TransferWithout || !got.Stale {
				t.Fatalf("flags %+v", got)
			}
			for _, key := range []string{"2026", "2026-Q3", "2026-10", "all_time", "", "rolling_wrong"} {
				if _, err := uc.Get(t.Context(), user, tc.typ, &key, domain.TransferWith); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("accepted key %q: %v", key, err)
				}
			}
		})
	}
}

func TestUnitRollingScreenNeverMixesAnOldSnapshotWithANewAnchor(t *testing.T) {
	from := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	first := from.AddDate(-2, 0, 0)
	stale := end.AddDate(0, 0, 14)
	snapshot := &domain.Snapshot{Range: &domain.TimeRange{Start: &from, End: &end}}
	state := &domain.State{HasData: true, DataFrom: &first, DataTo: &end, LastOperationAt: &from, StaleAfter: &stale, TimeZone: "UTC", CalculatedAt: &end}
	store := &screenStore{data: domain.AnalyticsData{State: state, Snapshot: snapshot}}
	uc, _ := NewScreen(store, func() time.Time { return end })
	read := func() domain.Analytics {
		t.Helper()
		got, err := uc.Get(t.Context(), user, 6, nil, domain.TransferWith)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if read().Snapshot != snapshot {
		t.Fatal("aligned snapshot lost")
	}
	nextEnd := end.AddDate(0, 0, 1)
	state.DataTo = &nextEnd
	for _, rebuilding := range []bool{false, true} {
		state.Recalculating = rebuilding
		got := read()
		if got.Snapshot != nil || !got.Range.End.Equal(nextEnd) {
			t.Fatalf("mixed boundaries %+v", got)
		}
	}
}

func TestUnitRollingInvalidStoredBoundaryIsInternalRatherThanClientValidation(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	store := &screenStore{data: domain.AnalyticsData{State: &domain.State{HasData: true, DataFrom: &from, DataTo: &end, LastOperationAt: &from, TimeZone: "UTC", CalculatedAt: &end}}}
	uc, _ := NewScreen(store, func() time.Time { return end })
	if _, err := uc.Get(t.Context(), user, domain.PeriodRollingQuarter, nil, domain.TransferWith); err == nil || errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("broken producer boundary was classified as user error: %v", err)
	}
}
