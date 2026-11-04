package metrics_test

import (
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationScreenUncoveredCalendarCivilBoundaries(t *testing.T) {
	for _, tc := range []struct {
		zone, key, start, end string
		typ                   domain.PeriodType
	}{
		{"America/Asuncion", "2017-10", "2017-10-01T04:00:00Z", "2017-11-01T03:00:00Z", domain.PeriodMonth},
		{"America/Asuncion", "2017-Q4", "2017-10-01T04:00:00Z", "2018-01-01T03:00:00Z", domain.PeriodQuarter},
		{"Africa/Cairo", "2014-08", "2014-07-31T22:00:00Z", "2014-08-31T21:00:00Z", domain.PeriodMonth},
		{"Asia/Kathmandu", "1986", "1985-12-31T18:30:00Z", "1986-12-31T18:15:00Z", domain.PeriodYear},
	} {
		t.Run(tc.zone+tc.key, func(t *testing.T) {
			e := start(t)
			zone, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			s := state(1)
			from := time.Date(2026, time.September, 1, 0, 0, 0, 0, zone)
			to := time.Date(2026, time.September, 26, 0, 0, 0, 0, zone)
			s.DataFrom, s.DataTo, s.TimeZone = &from, &to, tc.zone
			s.StaleAfter = moment(to.AddDate(0, 0, 14))
			user := userID(1)
			if _, err := e.uc.Upsert(t.Context(), domain.Upsert{UserID: user, Version: 1, State: s}); err != nil {
				t.Fatal(err)
			}
			screen, err := metrics.NewScreen(e.repo, func() time.Time { return to })
			if err != nil {
				t.Fatal(err)
			}
			got, err := screen.Get(t.Context(), user, tc.typ, &tc.key, domain.TransferWith)
			if err != nil {
				t.Fatal(err)
			}
			if got.Snapshot != nil || got.Previous != nil || got.Next != nil || got.Range.Start.UTC().Format(time.RFC3339) != tc.start || got.Range.End.UTC().Format(time.RFC3339) != tc.end {
				t.Fatalf("uncovered calendar range: %+v, want [%s,%s)", got, tc.start, tc.end)
			}
		})
	}
}
