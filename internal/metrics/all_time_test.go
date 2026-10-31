package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitAllTimeUsesPersistedBoundsAndStateFallbackWithoutCalendarNavigation(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2025, 12, 30, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, zone)
	stale := to.AddDate(0, 0, 14)
	now := to.Add(-time.Hour)
	state := &domain.State{HasData: true, DataFrom: &from, DataTo: &to, LastOperationAt: &now, StaleAfter: &stale,
		TimeZone: zone.String(), CalculatedAt: &now}
	f := &screenStore{data: domain.AnalyticsData{State: state, Previous: ptrString("2025"), Next: ptrString("2027")}}
	uc, err := NewScreen(f, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range domain.TransferModes {
		for _, key := range []*string{nil, ptrString("all_time")} {
			got, err := uc.Get(t.Context(), user, domain.PeriodAllTime, key, mode)
			if err != nil || f.key == nil || *f.key != "all_time" || got.Key.Key != "all_time" || got.Key.Mode != mode ||
				!got.Range.Start.Equal(from) || !got.Range.End.Equal(to) || got.Current || got.Previous != nil || got.Next != nil {
				t.Fatalf("state fallback %+v %v", got, err)
			}
		}
	}
	retained := from.AddDate(0, 0, 1)
	f.data.Snapshot = &domain.Snapshot{Range: &domain.TimeRange{Start: &retained, End: &to}}
	got, err := uc.Get(t.Context(), user, domain.PeriodAllTime, nil, domain.TransferWith)
	if err != nil || !got.Range.Start.Equal(retained) {
		t.Fatalf("retained facts lost their own range: %+v %v", got, err)
	}
	for _, key := range []string{"", "2026", "2026-Q4", "2026-10"} {
		before := f.calls
		if _, err := uc.Get(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith); !errors.Is(err, domain.ErrInvalid) || f.calls != before {
			t.Fatalf("invalid all_time key reached storage: %q %v", key, err)
		}
	}
	key := "all_time"
	if _, err := uc.Get(t.Context(), user, domain.PeriodMonth, &key, domain.TransferWith); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
}
