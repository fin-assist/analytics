package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitPeriodSelectionsKeepCalendarAndSingletonSemantics(t *testing.T) {
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	state := &domain.State{DataFrom: &from, DataTo: &end}
	for _, tc := range []struct {
		typ     domain.PeriodType
		key     string
		start   time.Time
		current bool
	}{
		{domain.PeriodMonth, "2026-10", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true},
		{domain.PeriodQuarter, "2026-Q4", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true},
		{domain.PeriodYear, "2026", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{domain.PeriodAllTime, "all_time", from, false},
		{domain.PeriodRollingYear, "rolling_year", time.Date(2025, 10, 31, 0, 0, 0, 0, time.UTC), false},
		{domain.PeriodRollingQuarter, "rolling_quarter", time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			selection := selectionFor(tc.typ)
			if key, err := selection.exact(tc.key); err != nil || key != tc.key {
				t.Fatalf("exact %q %v", key, err)
			}
			if err := selection.validateKey(tc.key); err != nil {
				t.Fatal(err)
			}
			start, finish, err := selection.bounds(tc.key, state, time.UTC)
			if err != nil || !start.Equal(tc.start) {
				t.Fatalf("bounds %v %v", start, err)
			}
			if got := selection.current(end.Add(-time.Hour), domain.TimeRange{Start: &start, End: &finish}); got != tc.current {
				t.Fatalf("current=%v", got)
			}
			for _, foreign := range []string{"all_time", "rolling_year", "rolling_quarter"} {
				if foreign == tc.key {
					continue
				}
				if _, err := selection.exact(foreign); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("foreign key %q: %v", foreign, err)
				}
			}
		})
	}
}
