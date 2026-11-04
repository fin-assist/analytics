package app_test

import (
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIUncoveredCalendarPeriodsUseIndependentCivilBoundaries(t *testing.T) {
	for _, tc := range []struct {
		zone, key, start, end string
		typ                   commonv1.PeriodType
	}{
		{"America/Asuncion", "2017-10", "2017-10-01T04:00:00Z", "2017-11-01T03:00:00Z", commonv1.PeriodType_PERIOD_TYPE_MONTH},
		{"America/Asuncion", "2017-Q4", "2017-10-01T04:00:00Z", "2018-01-01T03:00:00Z", commonv1.PeriodType_PERIOD_TYPE_QUARTER},
		{"Africa/Cairo", "2014-08", "2014-07-31T22:00:00Z", "2014-08-31T21:00:00Z", commonv1.PeriodType_PERIOD_TYPE_MONTH},
		{"Asia/Kathmandu", "1986", "1985-12-31T18:30:00Z", "1986-12-31T18:15:00Z", commonv1.PeriodType_PERIOD_TYPE_YEAR},
	} {
		t.Run(tc.zone+tc.key, func(t *testing.T) {
			_, c := startAPI(t)
			zone, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			seed := upsert(alice, 1, nil)
			seed.State.TimeZone = tc.zone
			seed.State.DataFrom = at(time.Date(2026, time.September, 1, 0, 0, 0, 0, zone))
			seed.State.DataTo = at(time.Date(2026, time.September, 26, 0, 0, 0, 0, zone))
			seed.State.StaleAfter = at(time.Date(2026, time.October, 10, 0, 0, 0, 0, zone))
			if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), seed); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
				req := &analyticsv1.GetAnalyticsRequest{Period: tc.typ, PeriodKey: proto.String(tc.key), TransferMode: mode}
				got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), req)
				if err != nil {
					t.Fatal(err)
				}
				if !got.HasData || got.TimeZone != tc.zone || got.Params.PeriodKey != tc.key || got.Params.TransferMode != mode || got.Period.Range.Start.AsTime().Format(time.RFC3339) != tc.start || got.Period.Range.End.AsTime().Format(time.RFC3339) != tc.end {
					t.Fatalf("uncovered period header: %v, want [%s,%s)", got, tc.start, tc.end)
				}
				if got.Period.Bounds.Coverage != commonv1.Coverage_COVERAGE_NO_DATA || got.Navigation.Previous != nil || got.Navigation.Next != nil || screenBlock(got, "tiles").Status != "none" {
					t.Fatalf("uncovered period gained financial data or navigation: %v", got)
				}
				other, err := c.analytics.GetAnalytics(apitest.User(t.Context(), bob), req)
				if err != nil || other.HasData || other.Params != nil || other.Period != nil {
					t.Fatalf("another user received calendar data: %v, %v", other, err)
				}
			}
		})
	}
}

func TestAPICalendarCivilResolutionRetainsStoredFinancialRangeDuringZoneChange(t *testing.T) {
	_, c := startAPI(t)
	const key = "2017-10"
	seed := upsert(alice, 1, both(key, 1, commonv1.Coverage_COVERAGE_PARTIAL))
	for _, snap := range seed.Periods {
		start, end := time.Date(2017, time.October, 1, 0, 0, 0, 0, time.UTC), time.Date(2017, time.November, 1, 0, 0, 0, 0, time.UTC)
		snap.Range = &commonv1.TimeRange{Start: at(start), End: at(end)}
		snap.Bounds.DataFrom, snap.Bounds.DataTo = at(start), at(start.Add(time.Hour))
	}
	setFixtureStateZone(t, seed.State, "UTC")
	seed.State.DataFrom = seed.Periods[0].Range.Start
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), seed); err != nil {
		t.Fatal(err)
	}
	update := upsert(alice, 2, nil)
	const zoneName = "America/Asuncion"
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		t.Fatal(err)
	}
	update.State.TimeZone = zoneName
	update.State.DataTo = at(time.Date(2026, time.September, 26, 0, 0, 0, 0, zone))
	update.State.Recalculating = true
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), update); err != nil {
		t.Fatal(err)
	}
	got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{PeriodKey: proto.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeZone != seed.State.TimeZone || !got.State.Recalculating || !proto.Equal(got.Period.Range, seed.Periods[0].Range) {
		t.Fatalf("civil resolution replaced the financial source calendar: %v", got)
	}
	tiles := screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)
	if tiles[1].(map[string]any)["value"] != float64(1) {
		t.Fatalf("stored expense changed: %v", tiles)
	}
}
