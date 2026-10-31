package domain

import (
	"errors"
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

func TestUnitStateAcceptsOnlyFirstRealCoveredDayEnd(t *testing.T) {
	for _, tc := range []struct{ zone, end string }{
		{"UTC", "2026-10-31T00:00:00Z"},
		{"America/Sao_Paulo", "2018-11-04T01:00:00-02:00"},
		{"America/Havana", "2020-11-01T00:00:00-04:00"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			end, err := time.Parse(time.RFC3339, tc.end)
			if err != nil {
				t.Fatal(err)
			}
			from := end.AddDate(0, -1, 0)
			stale := end.AddDate(0, 0, 14)
			state := &State{HasData: true, DataFrom: &from, DataTo: &end, LastOperationAt: &from, StaleAfter: &stale, CalculatedAt: &end, TimeZone: tc.zone}
			if err = state.Validate(); err != nil {
				t.Fatal(err)
			}
			later := end.Add(time.Hour)
			state.DataTo = &later
			if err = state.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("non-first day instant accepted: %v", err)
			}
		})
	}
}

func TestUnitRollingPeriodMetadataUsesExactBoundsAndRevisionIntersection(t *testing.T) {
	from := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	state := &State{DataFrom: &from, DataTo: &end, TimeZone: "UTC", Revisions: Revisions{"uploads": 2, "categories": 1}}
	span := &TimeRange{Start: &from, End: &end}
	snapshot := &Snapshot{Range: span, Coverage: CoverageComplete, Revisions: Revisions{"uploads": 1, "categories": 1}}
	got, err := RollingPeriods(PeriodRollingQuarter, state, snapshot)
	if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != CoverageComplete || len(got.Revisions) != 1 || got.Revisions["categories"] != 1 {
		t.Fatalf("coherent %+v %v", got, err)
	}
	next := end.AddDate(0, 0, 1)
	state.DataTo = &next
	for _, stored := range []*Snapshot{snapshot, nil} {
		got, err = RollingPeriods(PeriodRollingQuarter, state, stored)
		if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != CoveragePartial || len(got.Revisions) != 2 || got.Revisions["uploads"] != 2 {
			t.Fatalf("pending %+v %v", got, err)
		}
	}
	for _, missing := range []*TimeRange{nil, {}, {Start: &from}, {End: &end}} {
		if span.SameBounds(missing) || missing.SameBounds(span) {
			t.Fatal("absent bound certified")
		}
	}
	if !span.SameBounds(&TimeRange{Start: &from, End: &end}) {
		t.Fatal("equal bounds rejected")
	}
}
