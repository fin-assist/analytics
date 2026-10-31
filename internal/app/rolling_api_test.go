package app_test

import (
	"os"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func rollingRequest(t *testing.T) *analyticsv1.UpsertUserMetricsRequest {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAPIRollingFactsSlicesModesKeysAndIsolation(t *testing.T) {
	env, c := startAPI(t)
	req := rollingRequest(t)
	env.App.Now = func() time.Time { return req.State.CalculatedAt.AsTime() }
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []commonv1.PeriodType{commonv1.PeriodType_PERIOD_TYPE_ROLLING_YEAR, commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER} {
		key := "rolling_year"
		count := 13
		start := "2025-10-31T00:00:00+03:00"
		month := "2025-10"
		if kind == commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER {
			key = "rolling_quarter"
			count = 4
			start = "2026-07-31T00:00:00+03:00"
			month = "2026-07"
		}
		for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
			for _, date := range []*string{nil, proto.String(key)} {
				got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: kind, PeriodKey: date, TransferMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if got.Params.Period != kind || got.Params.PeriodKey != key || got.Params.TransferMode != mode || got.Period.IsCurrent || got.Navigation.Previous != nil || got.Navigation.Next != nil || len(got.Blocks) != 4 {
					t.Fatalf("header %v", got)
				}
				expected := float64(250)
				if mode == commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
					expected = 200
				}
				tiles := screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)
				if len(tiles) != 4 || tiles[1].(map[string]any)["value"] != expected {
					t.Fatalf("facts/modes %v", tiles)
				}
				chart := screenBlock(got, "monthly_chart").Data.AsMap()
				points := chart["points"].([]any)
				if len(points) != count || len(chart["notes"].([]any)) != 1 {
					t.Fatalf("chart %v", chart)
				}
				first := points[0].(map[string]any)
				if first["range"].(map[string]any)["from"] != start || first["value"] != float64(0) || first["action"].(map[string]any)["date"] != month {
					t.Fatalf("edge/zero %v", first)
				}
				gap := points[len(points)-2].(map[string]any)
				if gap["coverage"] != "no_data" || gap["action"] != nil || gap["value"] != nil {
					t.Fatalf("unknown month became zero %v", gap)
				}
				last := points[len(points)-1].(map[string]any)
				if last["range"].(map[string]any)["to"] != "2026-10-31T00:00:00+03:00" || last["value"] != expected {
					t.Fatalf("last slice %v", last)
				}
			}
		}
		listed, err := c.analytics.ListPeriods(apitest.User(t.Context(), alice), &analyticsv1.ListPeriodsRequest{Period: kind})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Periods) != 1 || listed.Periods[0].Key != key {
			t.Fatalf("singleton %v", listed)
		}
		for _, date := range []string{"2026-10", "2026", "all_time", "rolling_bad"} {
			_, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: kind, PeriodKey: &date})
			if status.Code(err) != codes.InvalidArgument || apitest.Reason(err) != "VALIDATION_ERROR" {
				t.Fatalf("invalid key %s %v", date, err)
			}
		}
		other, err := c.analytics.GetAnalytics(apitest.User(t.Context(), bob), &analyticsv1.GetAnalyticsRequest{Period: kind})
		if err != nil || other.GetHasData() {
			t.Fatalf("foreign data %v %v", other, err)
		}
		if _, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "user"), &analyticsv1.GetAnalyticsRequest{Period: kind, UserId: proto.String(alice)}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("foreign caller %v", err)
		}
	}
}

func TestAPIRollingPendingCoverageDailyReplacementAndRemoval(t *testing.T) {
	env, c := startAPI(t)
	req := rollingRequest(t)
	now := req.State.CalculatedAt.AsTime()
	env.App.Now = func() time.Time { return now }
	stateOnly := proto.Clone(req).(*analyticsv1.UpsertUserMetricsRequest)
	stateOnly.Periods = nil
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), stateOnly); err != nil {
		t.Fatal(err)
	}
	read := func() *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		out, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); !got.HasData || got.Period.Bounds.Coverage != commonv1.Coverage_COVERAGE_PARTIAL || screenBlock(got, "tiles").Status != "unavailable" {
		t.Fatalf("missing delivery fabricated facts %v", got)
	}
	req.CalcVersion++
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	before := read()
	if screenBlock(before, "tiles").Status != "ready" {
		t.Fatal(before)
	}
	// A clock-only read never moves the coverage anchor.
	now = now.AddDate(0, 1, 0)
	if got := read(); !proto.Equal(got.Period.Range, before.Period.Range) || got.Period.IsCurrent {
		t.Fatalf("clock moved anchor %v", got)
	}
	// A newer state may arrive before its period pair. Do not attach the old financial payload.
	stateOnly.CalcVersion = req.CalcVersion + 1
	stateOnly.State.DataTo = timestamppb.New(stateOnly.State.DataTo.AsTime().AddDate(0, 0, 1))
	stateOnly.State.Recalculating = true
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), stateOnly); err != nil {
		t.Fatal(err)
	}
	if got := read(); screenBlock(got, "tiles").Status != "unavailable" || got.Period.Range.End.AsTime().Equal(before.Period.Range.End.AsTime()) {
		t.Fatalf("old totals with new boundaries %v", got)
	}
	listed, err := c.analytics.ListPeriods(apitest.User(t.Context(), alice), &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER})
	if err != nil || len(listed.GetPeriods()) != 1 || !listed.Periods[0].Range.End.AsTime().Equal(stateOnly.State.DataTo.AsTime()) {
		t.Fatalf("list mismatch %v %v", listed, err)
	}
	req.CalcVersion = stateOnly.CalcVersion + 1
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	if screenBlock(read(), "tiles").Status != "ready" {
		t.Fatal("coherent replacement unavailable")
	}
	removed := &analyticsv1.UpsertUserMetricsRequest{UserId: alice, CalcVersion: req.CalcVersion + 1, State: &analyticsv1.UserDataState{TimeZone: "Europe/Moscow", CalculatedAt: timestamppb.New(now)}}
	for _, kind := range []commonv1.PeriodType{commonv1.PeriodType_PERIOD_TYPE_ROLLING_YEAR, commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER} {
		key := "rolling_year"
		if kind == 6 {
			key = "rolling_quarter"
		}
		removed.RemovedPeriods = append(removed.RemovedPeriods, &analyticsv1.PeriodRef{Period: kind, PeriodKey: key})
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), removed); err != nil {
		t.Fatal(err)
	}
	if read().HasData {
		t.Fatal("removed singleton retained")
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	if read().HasData {
		t.Fatal("old outbox resurrected removed source")
	}
}

func TestAPIRollingMidnightGapAndRepeatedMonthBoundary(t *testing.T) {
	cases := []struct{ zone, start, end, month, monthStart, monthEnd, sliceStart string }{
		{"America/Sao_Paulo", "2018-08-04T00:00:00-03:00", "2018-11-04T01:00:00-02:00", "2018-11", "2018-11-01T00:00:00-03:00", "2018-12-01T00:00:00-02:00", "2018-11-01T00:00:00-03:00"},
		{"America/Havana", "2020-11-01T00:00:00-04:00", "2021-02-01T00:00:00-05:00", "2020-11", "2020-11-01T00:00:00-04:00", "2020-12-01T00:00:00-05:00", "2020-11-01T00:00:00-04:00"},
		{"America/Havana", "2012-01-04T00:00:00-05:00", "2012-04-04T00:00:00-04:00", "2012-04", "2012-04-01T01:00:00-04:00", "2012-05-01T00:00:00-04:00", "2012-04-01T01:00:00-04:00"},
	}
	for _, tc := range cases {
		t.Run(tc.zone, func(t *testing.T) {
			parse := func(raw string) *timestamppb.Timestamp {
				t.Helper()
				at, err := time.Parse(time.RFC3339, raw)
				if err != nil {
					t.Fatal(err)
				}
				return timestamppb.New(at)
			}
			env, c := startAPI(t)
			req := rollingRequest(t)
			req.Periods = req.Periods[:2]
			req.State.TimeZone = tc.zone
			req.State.DataFrom = parse(tc.start)
			req.State.DataTo = parse(tc.end)
			req.State.CalculatedAt = parse(tc.end)
			req.State.LastOperationAt = parse(tc.start)
			req.State.StaleAfter = timestamppb.New(parse(tc.end).AsTime().AddDate(0, 0, 14))
			env.App.Now = func() time.Time { return parse(tc.end).AsTime() }
			for _, snap := range req.Periods {
				snap.Range = &commonv1.TimeRange{Start: parse(tc.start), End: parse(tc.end)}
				snap.Bounds.DataFrom = parse(tc.start)
				snap.Bounds.DataTo = timestamppb.New(parse(tc.end).AsTime().Add(-time.Second))
				snap.Bounds.Gaps = nil
				snap.CalculatedAt = parse(tc.end)
				for _, metric := range snap.Metrics {
					if metric.MetricId != "monthly_chart" {
						continue
					}
					sliceEnd := parse(tc.monthEnd)
					if sliceEnd.AsTime().After(parse(tc.end).AsTime()) {
						sliceEnd = parse(tc.end)
					}
					metric.Metric.Details = &analyticsv1.Metric_MonthlyChart{MonthlyChart: &analyticsv1.MonthlyChart{Points: []*analyticsv1.MonthlyPoint{{Month: tc.month, Range: &commonv1.TimeRange{Start: parse(tc.monthStart), End: parse(tc.monthEnd)}, CalculationRange: &commonv1.TimeRange{Start: parse(tc.sliceStart), End: sliceEnd}, ExpenseMinor: proto.Int64(0), IncomeMinor: proto.Int64(0), Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: parse(tc.sliceStart), DataTo: timestamppb.New(sliceEnd.AsTime().Add(-time.Second))}}}}}
				}
			}
			if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
				got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, TransferMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if !got.Period.Range.Start.AsTime().Equal(parse(tc.start).AsTime()) || !got.Period.Range.End.AsTime().Equal(parse(tc.end).AsTime()) || screenBlock(got, "monthly_chart").Status != "ready" {
					t.Fatalf("valid civil boundaries refused %v", got)
				}
				point := screenBlock(got, "monthly_chart").Data.AsMap()["points"].([]any)[0].(map[string]any)
				if point["range"].(map[string]any)["from"] != tc.sliceStart || point["action"].(map[string]any)["date"] != tc.month {
					t.Fatalf("civil slice or calendar identity changed %v", point)
				}
			}
		})
	}
}
