package metrics_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/go-grpc/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestIntegrationGetAnalyticsSnapshotTransport(t *testing.T) {
	tr := startTransport(t)
	exec := func(sql string) {
		t.Helper()
		if _, e := tr.db.Primary().Exec(t.Context(), sql); e != nil {
			t.Fatal(e)
		}
	}
	screenScenarios(t, tr.ingest, tr.analytics, metadata.AppendToOutgoingContext(t.Context(), "test-user", alice), t.Context(), metadata.AppendToOutgoingContext(t.Context(), "test-user", bob), t.Context(), exec)
}

func analyticsFixture(t *testing.T) *analyticsv1.UpsertUserMetricsRequest {
	t.Helper()
	raw, err := os.ReadFile("../../../../testdata/analytics.json")
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
	if _, err := client.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Supports: []*commonv1.BlockSupport{{Type: "chart", MaxVersion: 1}}}); status.Code(err) != codes.Unknown {
		// This transport intentionally omits the production error interceptor; the API suite checks INTERNAL.
		t.Fatalf("all requested blocks failed: %v", err)
	}
	for _, r := range []*analyticsv1.GetAnalyticsRequest{{Period: 99}, {TransferMode: 99}, {PeriodKey: proto.String("2026")}, {PeriodKey: proto.String("2026-99")}, {Period: 2, PeriodKey: proto.String("2026-Q8")}} {
		if _, e := client.GetAnalytics(userCtx, r); status.Code(e) != codes.InvalidArgument || server.Reason(e) != "VALIDATION_ERROR" {
			t.Fatalf("validation %+v: %v", r, e)
		}
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
