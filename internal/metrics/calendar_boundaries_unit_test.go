package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitCalendarRangeResolvesEachCivilBoundaryIndependently(t *testing.T) {
	for _, tc := range []struct {
		name, zone, key, start, end string
		typ                         domain.PeriodType
	}{
		{"midnight gap month", "America/Asuncion", "2017-10", "2017-10-01T04:00:00Z", "2017-11-01T03:00:00Z", domain.PeriodMonth},
		{"midnight gap quarter", "America/Asuncion", "2017-Q4", "2017-10-01T04:00:00Z", "2018-01-01T03:00:00Z", domain.PeriodQuarter},
		{"gap starts at one", "Africa/Cairo", "2014-08", "2014-07-31T22:00:00Z", "2014-08-31T21:00:00Z", domain.PeriodMonth},
		{"month after gap", "Africa/Cairo", "2014-09", "2014-08-31T21:00:00Z", "2014-09-30T22:00:00Z", domain.PeriodMonth},
		{"repeated midnight", "America/Havana", "2015-11", "2015-11-01T04:00:00Z", "2015-12-01T05:00:00Z", domain.PeriodMonth},
		{"year starts after gap", "Asia/Kathmandu", "1986", "1985-12-31T18:30:00Z", "1986-12-31T18:15:00Z", domain.PeriodYear},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			start, end, err := calendarRange(tc.typ, tc.key, zone)
			if err != nil {
				t.Fatal(err)
			}
			if start.UTC().Format(time.RFC3339) != tc.start || end.UTC().Format(time.RFC3339) != tc.end {
				t.Fatalf("range [%s,%s), want [%s,%s)", start.UTC(), end.UTC(), tc.start, tc.end)
			}
		})
	}
}

func TestUnitCalendarRangeRefusesUnrepresentableCivilYears(t *testing.T) {
	for _, tc := range []struct {
		typ domain.PeriodType
		key string
	}{
		{domain.PeriodMonth, "0000-01"},
		{domain.PeriodMonth, "9999-12"},
		{domain.PeriodQuarter, "9999-Q4"},
		{domain.PeriodYear, "9999"},
	} {
		if _, _, err := calendarRange(tc.typ, tc.key, time.UTC); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("%s %s: %v, want invalid range", tc.typ, tc.key, err)
		}
	}
	start, end, err := calendarRange(domain.PeriodMonth, "0001-01", time.UTC)
	if err != nil || start.UTC().Format(time.RFC3339) != "0001-01-01T00:00:00Z" || end.UTC().Format(time.RFC3339) != "0001-02-01T00:00:00Z" {
		t.Fatalf("first representable month: [%s,%s), %v", start, end, err)
	}
}
