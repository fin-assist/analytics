package app_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIGetAnalyticsScreenContract(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	conn, e := pgx.Connect(t.Context(), env.DSN)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	exec := func(sql string) {
		t.Helper()
		if _, e := conn.Exec(t.Context(), sql); e != nil {
			t.Fatal(e)
		}
	}
	screenScenarios(t, c.ingest, c.analytics, apitest.User(t.Context(), alice), producer(t.Context()), apitest.User(t.Context(), bob), apitest.Caller(t.Context(), "assistant"), exec)
}

func TestAPIGetAnalyticsAccessAndStorageFailure(t *testing.T) {
	env, c := startAPI(t)
	for _, caller := range []string{"bank-statement-data", "user"} {
		_, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), caller), &analyticsv1.GetAnalyticsRequest{UserId: proto.String(alice)})
		wantBare(t, err, codes.PermissionDenied)
	}
	_, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "bank-bff"), &analyticsv1.GetAnalyticsRequest{UserId: proto.String(alice)})
	if apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	_, err = c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{UserId: proto.String(bob)})
	wantBare(t, err, codes.InvalidArgument)
	_, err = c.analytics.GetAnalytics(apitest.Caller(t.Context(), "assistant"), &analyticsv1.GetAnalyticsRequest{})
	wantBare(t, err, codes.InvalidArgument)
	if _, err = c.ingest.UpsertUserMetrics(producer(t.Context()), analyticsFixture(t)); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if _, err = conn.Exec(t.Context(), `UPDATE period_snapshot SET range_start=NULL WHERE user_id=$1`, alice); err != nil {
		t.Fatal(err)
	}
	_, err = c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{})
	wantBare(t, err, codes.Internal)
	if strings.Contains(err.Error(), "snapshot") || strings.Contains(err.Error(), alice) {
		t.Fatalf("storage detail leaked: %v", err)
	}
}

func analyticsFixture(t *testing.T) *analyticsv1.UpsertUserMetricsRequest {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/analytics.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	return req
}
func screenBlock(resp *analyticsv1.GetAnalyticsResponse, id string) *commonv1.Block {
	for _, b := range resp.GetBlocks() {
		if b.Id == id {
			return b
		}
	}
	return nil
}
func screenMetric(req *analyticsv1.UpsertUserMetricsRequest, id string) *analyticsv1.Metric {
	for _, m := range req.Periods[0].Metrics {
		if m.MetricId == id {
			return m.Metric
		}
	}
	panic(id)
}
func screenScenarios(t *testing.T, ingest analyticsv1.MetricsIngestServiceClient, client analyticsv1.AnalyticsServiceClient, userCtx, producerCtx, otherCtx, systemCtx context.Context, exec func(string)) {
	t.Helper()
	req := analyticsFixture(t)
	write := func() {
		t.Helper()
		req.CalcVersion++
		if _, err := ingest.UpsertUserMetrics(producerCtx, req); err != nil {
			t.Fatal(err)
		}
	}
	read := func(r *analyticsv1.GetAnalyticsRequest) *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		resp, err := client.GetAnalytics(userCtx, r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	write()
	got := read(&analyticsv1.GetAnalyticsRequest{})
	if !got.HasData || got.Currency != "RUB" || got.TimeZone != "Europe/Moscow" || got.Params.PeriodKey != "2026-09" || got.Params.TransferMode != 1 || !got.Period.IsCurrent || got.State.Stale || got.State.UnreadLinesCount != 5 || got.State.CalculatedAt.AsTime() != req.Periods[0].CalculatedAt.AsTime() {
		t.Fatalf("screen header %+v", got)
	}
	tiles := screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)
	if tiles[0].(map[string]any)["value"] != float64(7258000) || tiles[1].(map[string]any)["id"] != "forecast" || tiles[2].(map[string]any)["value"] != float64(337280) || tiles[3].(map[string]any)["show_plus"] != true {
		t.Fatal(tiles)
	}
	if screenBlock(got, "regular_payments").Status != "tentative" || screenBlock(got, "regular_payments").Data.AsMap()["items_more"].(map[string]any)["value"] != float64(90000) {
		t.Fatal("regular payments")
	}
	if a := screenBlock(got, "bank_fees").Action; a.GetFilter().GetSelection() != "s_bank_fees" || a.GetFilter().GetSelectionName() != "Комиссии и проценты банку" {
		t.Fatal(a)
	}
	small := screenBlock(got, "small_frequent").Data.AsMap()
	if !strings.HasPrefix(small["description"].(string), "по августу ·") {
		t.Fatal(small)
	}
	if screenBlock(got, "monthly_chart").Data.AsMap()["points"].([]any)[1].(map[string]any)["data_to"] != "2026-09-25T21:40:00+03:00" {
		t.Fatal("point data_to")
	}
	got = read(&analyticsv1.GetAnalyticsRequest{TransferMode: 2})
	if screenBlock(got, "expense_categories").Data.AsMap()["total"].(map[string]any)["value"] != float64(8000000) {
		t.Fatal("mode selection")
	}
	if a := screenBlock(got, "bank_fees").Action; a.GetFilter().GetTransferMode() != 2 {
		t.Fatal("filter mode")
	}
	if resp, e := client.GetAnalytics(otherCtx, &analyticsv1.GetAnalyticsRequest{}); e != nil || resp.HasData || resp.Params != nil || resp.State != nil || resp.Currency != "" {
		t.Fatalf("tenant isolation %+v %v", resp, e)
	}
	if resp, e := client.GetAnalytics(systemCtx, &analyticsv1.GetAnalyticsRequest{UserId: proto.String(alice)}); e != nil || !resp.HasData {
		t.Fatalf("assistant %+v %v", resp, e)
	}
	other := analyticsFixture(t)
	other.UserId = bob
	for _, snap := range other.Periods {
		snap.PeriodKey = "2026-10"
		from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("MSK", 10800))
		snap.Range = &commonv1.TimeRange{Start: timestamppb.New(from), End: timestamppb.New(from.AddDate(0, 1, 0))}
		snap.Bounds.DataFrom = timestamppb.New(from)
		snap.Bounds.DataTo = timestamppb.New(from.AddDate(0, 0, 24))
		for _, m := range snap.Metrics {
			if m.MetricId == "tiles.expense" {
				m.Metric.ValueMinor = proto.Int64(9999900)
			}
		}
	}
	if _, err := ingest.UpsertUserMetrics(producerCtx, other); err != nil {
		t.Fatal(err)
	}
	own := read(&analyticsv1.GetAnalyticsRequest{})
	if own.Params.PeriodKey != "2026-09" || own.Navigation.Next != nil || screenBlock(own, "expense_categories").Data.AsMap()["total"].(map[string]any)["value"] != float64(8432000) {
		t.Fatalf("populated tenant contaminated default/navigation/metrics: %+v", own)
	}
	otherResp, err := client.GetAnalytics(otherCtx, &analyticsv1.GetAnalyticsRequest{PeriodKey: proto.String("2026-09")})
	if err != nil || otherResp.Period.GetBounds().GetCoverage() != commonv1.Coverage_COVERAGE_NO_DATA {
		t.Fatalf("other user's period leaked: %+v %v", otherResp, err)
	}
	got = read(&analyticsv1.GetAnalyticsRequest{Experiments: map[string]string{"analytics_order": "b"}})
	if got.Blocks[0].Id != "expense_categories" {
		t.Fatal("experiment")
	}
	got = read(&analyticsv1.GetAnalyticsRequest{Experiments: map[string]string{"analytics_order": "unknown", "future": "b"}})
	if got.Blocks[0].Id != "tiles" {
		t.Fatal("unknown experiment")
	}
	for _, typ := range []string{"heading", "chart", "ranked_list", "tile_group", "insight_card"} {
		got = read(&analyticsv1.GetAnalyticsRequest{Supports: []*commonv1.BlockSupport{{Type: typ, MaxVersion: 1}}})
		for _, b := range got.Blocks {
			if b.Type != typ || b.V != 1 {
				t.Fatal(b)
			}
		}
	}
	got = read(&analyticsv1.GetAnalyticsRequest{Supports: []*commonv1.BlockSupport{{Type: "future", MaxVersion: 9}, {Type: "chart", MaxVersion: 0}}})
	if len(got.Blocks) != 0 {
		t.Fatal("unsupported blocks")
	}
	got = read(&analyticsv1.GetAnalyticsRequest{PeriodKey: proto.String("2026-08")})
	if got.Period.Bounds.Coverage != commonv1.Coverage_COVERAGE_NO_DATA || got.Period.Bounds.DataFrom != nil || got.Navigation.GetNext() != "2026-09" || screenBlock(got, "tiles").Status != "unavailable" {
		t.Fatalf("no coverage %+v", got)
	}
	got = read(&analyticsv1.GetAnalyticsRequest{PeriodKey: proto.String("2026-10")})
	if got.Navigation.GetPrevious() != "2026-09" || got.Period.IsCurrent {
		t.Fatal("future navigation")
	}
	// Existing definition versions remain usable until the complete snapshot is replaced.
	req.Periods[0].Metrics[0].DefinitionVersion = -10
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(got, "expense_categories").Status != "ready" {
		t.Fatal("definition version gated")
	}
	// No financial formulas are run for the two larger period types.
	originals := append([]*analyticsv1.PeriodSnapshot(nil), req.Periods...)
	for _, tc := range []struct {
		typ             commonv1.PeriodType
		key, start, end string
	}{{2, "2026-Q3", "2026-07-01T00:00:00+03:00", "2026-10-01T00:00:00+03:00"}, {3, "2026", "2026-01-01T00:00:00+03:00", "2027-01-01T00:00:00+03:00"}} {
		for _, s := range originals {
			copy := proto.Clone(s).(*analyticsv1.PeriodSnapshot)
			copy.Period = tc.typ
			copy.PeriodKey = tc.key
			start, _ := time.Parse(time.RFC3339, tc.start)
			end, _ := time.Parse(time.RFC3339, tc.end)
			copy.Range = &commonv1.TimeRange{Start: timestamppb.New(start), End: timestamppb.New(end)}
			req.Periods = append(req.Periods, copy)
		}
		write()
		got = read(&analyticsv1.GetAnalyticsRequest{Period: tc.typ, PeriodKey: proto.String("2026-09")})
		if got.Params.PeriodKey != tc.key || screenBlock(got, "bank_fees") != nil {
			t.Fatalf("exact period %+v", got)
		}
		if tc.typ == 3 && screenBlock(got, "balance") != nil {
			t.Fatal("year balance")
		}
	}
	req.Periods = originals
	// Every stored metric state is preserved. Lock text is rebuilt, never trusted from the producer.
	for _, reason := range []analyticsv1.LockReason{1, 2, 3, 4, 99} {
		req = analyticsFixture(t)
		req.CalcVersion = 20 + int32(reason)
		for _, s := range req.Periods {
			for _, ms := range s.Metrics {
				if ms.MetricId == "monthly_chart" || strings.HasPrefix(ms.MetricId, "insights.") || ms.MetricId == "tiles.forecast" {
					ms.Metric.Status = 3
					ms.Metric.Lock = &analyticsv1.MetricLock{Reason: reason, Required: proto.Int32(3), Available: proto.Int32(0), AvailableFrom: req.State.DataTo, Text: "producer text must not escape"}
					ms.Metric.ValueMinor = nil
				}
			}
		}
		write()
		got = read(&analyticsv1.GetAnalyticsRequest{})
		b := screenBlock(got, "regular_payments")
		if b.Status != "locked" || b.GetLock().GetText() == "" || strings.Contains(b.GetLock().GetText(), "producer text") {
			t.Fatal(b)
		}
		if len(b.Data.AsMap()) != 2 {
			t.Fatal("locked card exposed values")
		}
	}
	// A newer version is used after the unknown-enum scenario above.
	req = analyticsFixture(t)
	req.CalcVersion = 200
	for _, s := range req.Periods {
		for _, m := range s.Metrics {
			m.Metric.Status = 4
			m.Metric.ValueMinor = nil
		}
	}
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	for _, id := range []string{"expense_categories", "income_categories", "regular_payments", "notable_spending", "bank_fees", "small_frequent"} {
		b := screenBlock(got, id)
		if b.Status != "none" || b.GetEmptyText() == "" {
			t.Fatal(b)
		}
	}
	req = analyticsFixture(t)
	req.CalcVersion = 210
	for _, s := range req.Periods {
		for _, m := range s.Metrics {
			if m.MetricId == "monthly_chart" {
				m.Metric.Status = 3
				m.Metric.Lock = &analyticsv1.MetricLock{Reason: 2, Required: proto.Int32(2), Available: proto.Int32(1)}
			}
		}
	}
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(got, "monthly_chart") != nil {
		t.Fatal("early chart")
	}
	// Stale/current are evaluated at read time, even when the stored forecast was ready.
	req = analyticsFixture(t)
	req.CalcVersion = 220
	req.State.StaleAfter = req.State.DataTo
	req.State.StaleAfter = timestamppb.New(req.State.DataTo.AsTime().Add(time.Hour))
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if !got.State.Stale || screenBlock(got, "tiles").Data.AsMap()["tiles"].([]any)[1].(map[string]any)["id"] != "expense" {
		t.Fatal("stale fact fallback")
	}
	req = analyticsFixture(t)
	req.CalcVersion = 230
	screenMetric(req, "tiles.balance").Details = &analyticsv1.Metric_Balance{Balance: &analyticsv1.BalanceDetails{ShareOfIncome: proto.Float64(-0.12)}}
	screenMetric(req, "tiles.income").ValueMinor = proto.Int64(1 << 53)
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(got, "tiles").Status != "unavailable" || screenBlock(got, "bank_fees").Status != "ready" {
		t.Fatal("unsafe number did not degrade only its block")
	}
	// Database corruption is isolated to one block when its typed details cannot be decoded.
	req = analyticsFixture(t)
	req.CalcVersion = 240
	write()
	exec(`UPDATE period_metric SET details='{"categories":"broken"}'::jsonb WHERE user_id='01a107db-ae80-7b07-b8a1-00000000a11c' AND metric_id='expense_categories'`)
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(got, "expense_categories").Status != "unavailable" || screenBlock(got, "income_categories").Status != "ready" {
		t.Fatal("partial failure")
	}
	exec(`UPDATE period_metric SET details='{"monthly_chart":"broken"}'::jsonb WHERE user_id='01a107db-ae80-7b07-b8a1-00000000a11c' AND metric_id='monthly_chart'`)
	healthy := read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(healthy, "monthly_chart").Status != "unavailable" || screenBlock(healthy, "balance").Status != "ready" || len(screenBlock(healthy, "balance").Data.AsMap()["points"].([]any)) != 0 {
		t.Fatal("monthly chart failure suppressed healthy balance summary")
	}
	exec(`UPDATE period_metric SET details='{"balance":"broken"}'::jsonb WHERE user_id='01a107db-ae80-7b07-b8a1-00000000a11c' AND metric_id='tiles.balance'`)
	if _, err := client.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Supports: []*commonv1.BlockSupport{{Type: "chart", MaxVersion: 1}}}); status.Code(err) != codes.Internal {
		t.Fatalf("all requested blocks failed: %v", err)
	}
	for _, r := range []*analyticsv1.GetAnalyticsRequest{{Period: 99}, {TransferMode: 99}, {PeriodKey: proto.String("2026")}, {PeriodKey: proto.String("2026-99")}, {Period: 2, PeriodKey: proto.String("2026-Q8")}} {
		if _, e := client.GetAnalytics(userCtx, r); status.Code(e) != codes.InvalidArgument || apitest.Reason(e) != "VALIDATION_ERROR" {
			t.Fatalf("validation %+v: %v", r, e)
		}
	}
	req = analyticsFixture(t)
	req.CalcVersion = 245
	req.State.TimeZone = "UTC"
	req.State.Recalculating = true
	write()
	rebuilt := read(&analyticsv1.GetAnalyticsRequest{})
	assertRebuiltCalendar(t, rebuilt, req.Periods[0])
	// The opposite direction would shift an exclusive end into the next month in the new zone.
	rawUTC, e := os.ReadFile("../../testdata/analytics.json")
	if e != nil {
		t.Fatal(e)
	}
	req = &analyticsv1.UpsertUserMetricsRequest{}
	if e = protojson.Unmarshal([]byte(strings.ReplaceAll(string(rawUTC), "+03:00", "Z")), req); e != nil {
		t.Fatal(e)
	}
	req.CalcVersion = 247
	req.State.TimeZone = "Europe/Moscow"
	req.State.Recalculating = true
	write()
	rebuilt = read(&analyticsv1.GetAnalyticsRequest{})
	assertRebuiltCalendar(t, rebuilt, req.Periods[0])
	// A missing group filter must retain a valid metric-level transition.
	smallMetric := screenMetric(req, "insights.small_frequent")
	smallMetric.Filters = proto.Clone(smallMetric.GetSmallFrequent().Items[0].Filters).(*commonv1.OperationsFilter)
	smallMetric.Filters.Selection = proto.String("s_small_metric")
	smallMetric.GetSmallFrequent().Items[0].Filters = nil
	req.CalcVersion = 248
	write()
	rebuilt = read(&analyticsv1.GetAnalyticsRequest{})
	if screenBlock(rebuilt, "small_frequent").Action.GetFilter().GetSelection() != "s_small_metric" {
		t.Fatal("metric fallback action erased")
	}
	// Empty statements are distinct from missing snapshots while the user has other operations.
	req = analyticsFixture(t)
	req.CalcVersion = 250
	req.State.HasData = false
	write()
	got = read(&analyticsv1.GetAnalyticsRequest{})
	if got.HasData || got.Params != nil || got.State != nil || len(got.Blocks) > 0 {
		t.Fatalf("empty %+v", got)
	}
}

func assertRebuiltCalendar(t *testing.T, r *analyticsv1.GetAnalyticsResponse, source *analyticsv1.PeriodSnapshot) {
	t.Helper()
	tiles := screenBlock(r, "tiles").Data.AsMap()["tiles"].([]any)
	if tiles[1].(map[string]any)["caption"] != "на сентябрь" || tiles[2].(map[string]any)["caption"] != "в августе — 2 890 ₽" {
		t.Fatal("tile calendar shifted", tiles)
	}
	if !strings.HasPrefix(screenBlock(r, "small_frequent").Data.AsMap()["description"].(string), "по августу ·") || !strings.HasSuffix(screenBlock(r, "regular_payments").Data.AsMap()["description"].(string), "с июня") {
		t.Fatal("observation basis shifted")
	}
	if !strings.Contains(screenBlock(r, "notable_spending").Data.AsMap()["description"].(string), "(июнь — август)") {
		t.Fatal("notable basis shifted")
	}
	chart := screenBlock(r, "monthly_chart")
	if !strings.Contains(chart.GetAltText(), "август 70 000 ₽, сентябрь 84 320 ₽") {
		t.Fatal(chart.GetAltText())
	}
	notes := chart.Data.AsMap()["notes"].([]any)
	if !strings.Contains(notes[1].(map[string]any)["text"].(string), "июнь — август") {
		t.Fatal(notes)
	}
	if screenBlock(r, "expense_categories").Data.AsMap()["total"].(map[string]any)["label"] != "Всего · 1–25 сентября" {
		t.Fatal("covered-day caption shifted")
	}
	if !proto.Equal(r.Period.Range, source.Range) || !proto.Equal(r.Period.Bounds.DataFrom, source.Bounds.DataFrom) || !proto.Equal(r.Period.Bounds.DataTo, source.Bounds.DataTo) {
		t.Fatal("stored financial boundaries changed")
	}
	point := chart.Data.AsMap()["points"].([]any)[1].(map[string]any)
	instant, e := time.Parse(time.RFC3339Nano, point["data_to"].(string))
	if e != nil || !instant.Equal(source.Bounds.DataTo.AsTime()) || point["note"] != "по 25-е" {
		t.Fatal("point instant/note changed", point, e)
	}
}
