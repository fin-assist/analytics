package app_test

import (
	"os"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIChartPagesKeepAllMonthsAndRejectForeignOrStaleCursors(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	zone, _ := time.LoadLocation("Europe/Moscow")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, zone)
	env.App.Now = func() time.Time { return now }
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	batch := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, batch); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2010, 1, 1, 0, 0, 0, 0, zone)
	end := start.AddDate(0, 121, 0)
	batch.State.DataFrom = at(start)
	batch.State.DataTo = at(end)
	batch.State.LastOperationAt = at(end.Add(-time.Hour))
	batch.State.StaleAfter = at(end.AddDate(0, 0, 14))
	batch.State.FullMonths = 121
	for _, snapshot := range batch.Periods {
		snapshot.Range = &commonv1.TimeRange{Start: at(start), End: at(end)}
		snapshot.Bounds = &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE, DataFrom: at(start), DataTo: at(end.Add(-time.Second))}
		for _, metric := range snapshot.Metrics {
			if metric.MetricId == "monthly_chart" {
				points := make([]*analyticsv1.MonthlyPoint, 121)
				for i := range points {
					a := start.AddDate(0, i, 0)
					b := a.AddDate(0, 1, 0)
					points[i] = &analyticsv1.MonthlyPoint{Month: a.Format("2006-01"), Range: &commonv1.TimeRange{Start: at(a), End: at(b)}, ExpenseMinor: proto.Int64(0), IncomeMinor: proto.Int64(0), Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_COMPLETE}}
				}
				metric.Metric.Details = &analyticsv1.Metric_MonthlyChart{MonthlyChart: &analyticsv1.MonthlyChart{Points: points}}
			}
		}
	}
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), batch); err != nil {
		t.Fatal(err)
	}
	// Full user and assistant API reads concurrently issue the initial pointer.
	type reply struct {
		screen *analyticsv1.GetAnalyticsResponse
		err    error
	}
	done := make(chan reply, 12)
	for i := range 12 {
		go func() {
			callctx := apitest.User(ctx, alice)
			if i%2 == 1 {
				callctx = apitest.Caller(ctx, "assistant")
			}
			r, e := c.analytics.GetAnalytics(callctx, &analyticsv1.GetAnalyticsRequest{UserId: proto.String(alice), Period: 4, PeriodKey: proto.String("all_time"), TransferMode: 1})
			done <- reply{r, e}
		}()
	}
	var screen *analyticsv1.GetAnalyticsResponse
	for range 12 {
		got := <-done
		if got.err != nil {
			t.Fatal(got.err)
		}
		if screen == nil {
			screen = got.screen
		} else if !proto.Equal(screen, got.screen) {
			t.Fatal("concurrent screen generations differ")
		}
	}
	chart := screenBlock(screen, "monthly_chart")
	if chart == nil {
		t.Fatal("missing navigation")
	}
	d := chart.Data.AsMap()
	if len(d["points"].([]any)) != 60 || screen.TimeZone != "Europe/Moscow" {
		t.Fatal(d)
	}
	meta := d["page"].(map[string]any)
	cursor := meta["previous"].(string)
	rev := meta["revision"]
	page, err := c.analytics.GetChartPage(apitest.User(ctx, alice), &analyticsv1.GetChartPageRequest{Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	pd := page.Block.Data.AsMap()
	pm := pd["page"].(map[string]any)
	if len(pd["points"].([]any)) != 60 || pm["revision"] != rev || page.TimeZone != "Europe/Moscow" {
		t.Fatal(page)
	}
	oldest, err := c.analytics.GetChartPage(apitest.User(ctx, alice), &analyticsv1.GetChartPageRequest{Cursor: pm["previous"].(string)})
	if err != nil || len(oldest.Block.Data.AsMap()["points"].([]any)) != 1 {
		t.Fatal(oldest, err)
	}
	forward, err := c.analytics.GetChartPage(apitest.User(ctx, alice), &analyticsv1.GetChartPageRequest{Cursor: pm["next"].(string)})
	if err != nil || len(forward.Block.Data.AsMap()["points"].([]any)) != 60 {
		t.Fatal(forward, err)
	}
	_, err = c.analytics.GetChartPage(apitest.User(ctx, bob), &analyticsv1.GetChartPageRequest{Cursor: cursor})
	if apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatal("foreign cursor", err)
	}
	_, err = c.analytics.GetChartPage(apitest.Caller(ctx, "assistant"), &analyticsv1.GetChartPageRequest{Cursor: cursor})
	wantBare(t, err, codes.PermissionDenied)
	now = now.AddDate(0, 0, 1)
	_, err = c.analytics.GetChartPage(apitest.User(ctx, alice), &analyticsv1.GetChartPageRequest{Cursor: cursor})
	rest, ok := finerr.ToREST(err)
	if !ok || rest.Code != "VALIDATION_ERROR" || len(rest.Violations) != 1 || rest.Violations[0].Reason != "OUT_OF_RANGE" {
		t.Fatal("stale cursor", err)
	}
	for _, badMonth := range []bool{true, false} {
		// Acquire a live cursor, then deliver a newer source version before corrupting its chart.
		batch.CalcVersion++
		if _, err = c.ingest.UpsertUserMetrics(producer(ctx), batch); err != nil {
			t.Fatal(err)
		}
		fresh, e := c.analytics.GetAnalytics(apitest.User(ctx, alice), &analyticsv1.GetAnalyticsRequest{Period: 4, TransferMode: 1})
		if e != nil {
			t.Fatal(e)
		}
		cursor = screenBlock(fresh, "monthly_chart").Data.AsMap()["page"].(map[string]any)["previous"].(string)
		batch.CalcVersion++
		delivery := proto.Clone(batch).(*analyticsv1.UpsertUserMetricsRequest)
		if badMonth {
			for _, snapshot := range delivery.Periods {
				for _, metric := range snapshot.Metrics {
					if metric.MetricId == "monthly_chart" {
						metric.Metric.GetMonthlyChart().Points[0].Month = ""
					}
				}
			}
		}
		if _, err = c.ingest.UpsertUserMetrics(producer(ctx), delivery); err != nil {
			t.Fatal(err)
		}
		if !badMonth {
			// Non-array details are a corrupt stored payload, not a valid protobuf representation.
			if _, err = env.App.DB.Primary().Exec(ctx, `UPDATE period_metric SET details=jsonb_set(details,'{monthly_chart,points}','{}'::jsonb) WHERE user_id=$1 AND period_type=4 AND metric_id='monthly_chart'`, alice); err != nil {
				t.Fatal(err)
			}
		}
		_, err = c.analytics.GetChartPage(apitest.User(ctx, alice), &analyticsv1.GetChartPageRequest{Cursor: cursor})
		rest, ok = finerr.ToREST(err)
		if !ok || rest.Code != "VALIDATION_ERROR" || len(rest.Violations) != 1 || rest.Violations[0].Field != "cursor" || rest.Violations[0].Reason != "OUT_OF_RANGE" {
			t.Fatalf("degraded source (badMonth=%v) must invalidate cursor: %v", badMonth, err)
		}
	}
}
