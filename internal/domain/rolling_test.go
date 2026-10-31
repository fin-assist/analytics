package domain

import (
	"testing"
	"time"
)

func TestUnitRollingRangeValidationAndDomainKeys(t *testing.T) {
	zone := time.UTC
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, zone)
	for _, typ := range []PeriodType{PeriodRollingYear, PeriodRollingQuarter} {
		if !typ.Known() || !typ.Rolling() || (PeriodKey{Type: typ, Key: typ.String()}).Validate() != nil {
			t.Fatalf("unknown rolling type%d", typ)
		}
		for _, key := range []string{"2026", "2026-10", "all_time", "rolling_wrong", ""} {
			if (PeriodKey{Type: typ, Key: key}).Validate() == nil {
				t.Fatalf("accepted %q", key)
			}
		}
		if _, err := RollingRange(typ, &end, zone); err != nil {
			t.Fatal(err)
		}
	}
	notMidnight := end.Add(time.Hour)
	ancient := time.Date(1, 1, 1, 0, 0, 0, 0, zone)
	for _, tc := range []struct {
		typ  PeriodType
		end  *time.Time
		zone *time.Location
	}{{PeriodMonth, &end, zone}, {PeriodRollingYear, nil, zone}, {PeriodRollingYear, &end, nil}, {PeriodRollingYear, &notMidnight, zone}, {PeriodRollingYear, &ancient, zone}} {
		if _, err := RollingRange(tc.typ, tc.end, tc.zone); err == nil {
			t.Fatalf("invalid boundary accepted%+v", tc)
		}
	}
}

func TestUnitRollingRangeStartsAtFirstRealInstantAcrossMidnightTransitions(t *testing.T) {
	for _, tc := range []struct {
		zone, end, start string
		typ              PeriodType
	}{
		{"America/Sao_Paulo", "2018-11-04T01:00:00-02:00", "2018-08-04T00:00:00-03:00", PeriodRollingQuarter},
		{"America/Havana", "2020-11-01T00:00:00-04:00", "2020-08-01T00:00:00-04:00", PeriodRollingQuarter},
		{"America/Sao_Paulo", "2019-02-04T00:00:00-02:00", "2018-11-04T01:00:00-02:00", PeriodRollingQuarter},
		{"Pacific/Apia", "2012-03-30T00:00:00+14:00", "2011-12-31T00:00:00+14:00", PeriodRollingQuarter},
		{"Pacific/Kiritimati", "1995-03-31T00:00:00+14:00", "1995-01-01T00:00:00+14:00", PeriodRollingQuarter},
	} {
		zone, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Fatal(err)
		}
		end, err := time.Parse(time.RFC3339, tc.end)
		if err != nil {
			t.Fatal(err)
		}
		start, err := time.Parse(time.RFC3339, tc.start)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RollingRange(tc.typ, &end, zone)
		if err != nil || !got.Start.Equal(start) || !got.End.Equal(end) {
			t.Fatalf("zone%s end%s: %+v %v", tc.zone, tc.end, got, err)
		}
	}
}

func TestUnitCivilDayStartHandlesMissingDatesAndInvalidInputs(t *testing.T) {
	if first, ok := CivilDayStart(1, 1, 1, time.UTC); !ok || first.Year() != 1 || first.Month() != 1 || first.Day() != 1 {
		t.Fatal("valid minimum Gregorian date lost")
	}
	zone, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := CivilDayStart(2011, 12, 30, zone); ok {
		t.Fatal("skipped civil day invented")
	}
	for _, tc := range []struct {
		year  int
		month time.Month
		day   int
		zone  *time.Location
	}{{2026, 2, 30, time.UTC}, {0, 1, 1, time.UTC}, {2026, 1, 1, nil}} {
		if _, ok := CivilDayStart(tc.year, tc.month, tc.day, tc.zone); ok {
			t.Fatalf("invalid date accepted%+v", tc)
		}
	}
}
