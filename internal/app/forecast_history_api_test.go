package app_test

import (
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIFactAndForecastHistoryAcrossCalendarYearAndDelivery(t *testing.T) {
	env, c := startAPI(t)
	current := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	env.App.Now = func() time.Time { return current }
	raw, err := os.ReadFile("../../testdata/forecast_history.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	write := func() {
		t.Helper()
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026")})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	forecastText := func(got *analyticsv1.GetAnalyticsResponse) string {
		t.Helper()
		tiles := screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)
		if len(tiles) != 5 || tiles[1].(map[string]any)["id"] != "expense" || tiles[1].(map[string]any)["value"] != float64(100000) {
			t.Fatal(tiles)
		}
		return tiles[4].(map[string]any)["lock"].(map[string]any)["text"].(string)
	}
	write()
	text := forecastText(read())
	for _, want := range []string{"есть 11", "независимо от выбранного года", "ноябрь 2025", "сентябрь 2026", "нет данных за 10–12 октября", "октябрь 2025 (данные за 30–31 октября)", "октябрь 2026 (данные за 1–30 октября; месяц ещё не закончился; нет данных за 10–12 октября)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	without, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026"), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT})
	if err != nil {
		t.Fatal(err)
	}
	if other := forecastText(without); other != text {
		t.Fatalf("transfer mode lost the calendar history: %s", other)
	}
	current = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	text = forecastText(read())
	if strings.Contains(text, "Учтены полные месяцы:") || strings.Contains(text, "ещё не закончился") {
		t.Fatal("old state claimed current readiness", text)
	}
	req.Periods = nil
	req.CalcVersion = 5
	req.State.CalculatedAt = timestamppb.New(current)
	write()
	text = forecastText(read())
	if !strings.Contains(text, "Учтены полные месяцы:") || strings.Contains(text, "ещё не закончился") {
		t.Fatal("daily state did not restore ended calendar", text)
	}
	other, err := c.analytics.GetAnalytics(apitest.User(t.Context(), bob), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026")})
	if err != nil || other.HasData || len(other.Blocks) != 0 {
		t.Fatalf("foreign data %+v %v", other, err)
	}
	req.Periods = nil
	req.CalcVersion = 6
	req.State.Recalculating = true
	write()
	text = forecastText(read())
	if strings.Contains(text, "Учтены полные месяцы:") || !strings.Contains(text, "данными за каждый день") {
		t.Fatal(text)
	}
	req.CalcVersion = 7
	req.State.Recalculating = false
	write()
	if text = forecastText(read()); !strings.Contains(text, "ноябрь 2025") {
		t.Fatal(text)
	}
	req.CalcVersion = 8
	req.RemovedPeriods = []*analyticsv1.PeriodRef{{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2025-11"}}
	write()
	text = forecastText(read())
	if strings.Contains(text, "Учтены полные месяцы:") {
		t.Fatal("missing snapshot invented a full basis", text)
	}
	got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2024")})
	if err != nil {
		t.Fatal(err)
	}
	empty := screenBlock(got, "tiles")
	if empty.Status != "none" || len(empty.Data.AsMap()["tiles"].([]any)) != 0 || empty.GetEmptyText() != "За этот период данных нет" {
		t.Fatal(empty)
	}
}

func TestAPIDailyRolloverExplainsRetainedPartialMonthWithoutImport(t *testing.T) {
	env, c := startAPI(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	env.App.Now = func() time.Time { return now }
	raw, err := os.ReadFile("../../testdata/forecast_history.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	var keep []*analyticsv1.PeriodSnapshot
	var october []*analyticsv1.PeriodSnapshot
	for _, period := range req.Periods {
		if period.PeriodKey == "2026-09" || period.PeriodKey == "2026-10" || period.PeriodKey == "2026" {
			keep = append(keep, period)
			if period.PeriodKey == "2026-10" {
				october = append(october, period)
			}
			if period.PeriodKey == "2026" {
				period.Metrics[len(period.Metrics)-1].Metric.Lock.Available = proto.Int32(2)
			}
		}
	}
	req.Periods = keep
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, from.Location())
	for _, period := range october {
		period.Bounds.DataTo = timestamppb.New(end.Add(-time.Second))
		period.Bounds.Gaps = nil
	}
	req.State.DataFrom = timestamppb.New(from)
	req.State.DataTo = timestamppb.New(end)
	req.State.FullMonths = 2
	req.State.CalculatedAt = timestamppb.New(now)
	// October remains PARTIAL as received before midnight, with exactly the same source revisions.
	if october[0].Bounds.Coverage != commonv1.Coverage_COVERAGE_PARTIAL {
		t.Fatal("fixture must retain partial header")
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026")})
	if err != nil {
		t.Fatal(err)
	}
	tiles := screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)
	text := tiles[4].(map[string]any)["lock"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Учтены полные месяцы: сентябрь 2026 — октябрь 2026.") || strings.Contains(text, "Неполные месяцы:") {
		t.Fatal(text)
	}
}

func TestAPIKnownSinglePartialMonthKeepsExplicitNoDataEmptyAndEarly(t *testing.T) {
	env, c := startAPI(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	env.App.Now = func() time.Time { return now }
	raw, err := os.ReadFile("../../testdata/forecast_history.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	to := from.AddDate(0, 0, 7)
	var months []*analyticsv1.PeriodSnapshot
	for _, period := range req.Periods {
		if period.PeriodKey == "2026-10" {
			period.Bounds.DataTo = timestamppb.New(to.Add(-time.Second))
			period.Bounds.Gaps = nil
			period.CalculatedAt = timestamppb.New(now)
			months = append(months, period)
		}
		if period.PeriodKey == "2026" {
			period.Bounds.DataFrom = timestamppb.New(from)
			period.Bounds.DataTo = timestamppb.New(to.Add(-time.Second))
			period.Metrics[len(period.Metrics)-1].Metric.Lock.Available = nil
			months = append(months, period)
		}
	}
	req.Periods = months
	req.State.DataFrom = timestamppb.New(from)
	req.State.DataTo = timestamppb.New(to)
	req.State.FullMonths = 0
	req.State.CalculatedAt = timestamppb.New(now)
	req.State.LastOperationAt = timestamppb.New(from)
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	legacy, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026")})
	if err != nil {
		t.Fatal(err)
	}
	legacyTiles := screenBlock(legacy, "tiles").Data.AsMap()["tiles"].([]any)
	legacyText := legacyTiles[4].(map[string]any)["lock"].(map[string]any)["text"].(string)
	if strings.Contains(legacyText, "Учтены полные месяцы:") {
		t.Fatal("unknown available fabricated zero", legacyText)
	}
	response, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{PeriodKey: proto.String("2025-09")})
	if err != nil {
		t.Fatal(err)
	}
	if screenBlock(response, "monthly_chart") != nil || response.Navigation.GetPrevious() != "" || response.Navigation.GetNext() != "" {
		t.Fatal("no-data/early contract", response)
	}
	tiles := screenBlock(response, "tiles")
	if tiles.Status != "none" || len(tiles.Data.AsMap()["tiles"].([]any)) != 0 {
		t.Fatal(tiles)
	}
}
