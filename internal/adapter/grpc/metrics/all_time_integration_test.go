package metrics_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestIntegrationAllTimeSnapshotsPersistModesCoverageAndRemovalWatermarks(t *testing.T) {
	tr := startTransport(t)
	raw, err := os.ReadFile("../../../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "test-user", alice)
	// The pre-upgrade producer publishes only state/calendar snapshots. Reading all_time
	// must preserve its factual envelope while waiting for the new seven-family snapshot.
	stateOnly := proto.Clone(req).(*analyticsv1.UpsertUserMetricsRequest)
	stateOnly.Periods = nil
	stateOnly.CalcVersion--
	if _, err = tr.ingest.UpsertUserMetrics(t.Context(), stateOnly); err != nil {
		t.Fatal(err)
	}
	read := func(mode commonv1.TransferMode, v int32) *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		r, e := tr.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 4, TransferMode: mode, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: v}, {Type: "chart", MaxVersion: 1}, {Type: "ranked_list", MaxVersion: 1}}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	from, _ := time.Parse(time.RFC3339, "2025-12-30T00:00:00+03:00")
	to, _ := time.Parse(time.RFC3339, "2026-10-31T00:00:00+03:00")
	for _, mode := range []commonv1.TransferMode{1, 2} {
		for _, version := range []int32{1, 2} {
			pending := read(mode, version)
			if !pending.HasData || pending.Params.Period != 4 || pending.Params.PeriodKey != "all_time" || pending.Params.TransferMode != mode || pending.Period.IsCurrent || pending.Navigation.Previous != nil || pending.Navigation.Next != nil || !pending.Period.Range.Start.AsTime().Equal(from) || !pending.Period.Range.End.AsTime().Equal(to) || !pending.State.DataFrom.AsTime().Equal(from) || !pending.State.DataTo.AsTime().Equal(to) {
				t.Fatal(pending)
			}
			if pending.Period.Bounds.Coverage != 2 || !pending.Period.Bounds.DataFrom.AsTime().Equal(from) || !pending.Period.Bounds.DataTo.AsTime().Equal(to.Add(-time.Nanosecond)) {
				t.Fatal(pending.Period.Bounds)
			}
			b := screenBlock(pending, "tiles")
			key := "tiles"
			if version == 2 {
				key = "groups"
			}
			if b.V != version || b.Status != "unavailable" || len(b.Data.AsMap()[key].([]any)) != 0 {
				t.Fatal(b)
			}
		}
	}
	write, err := tr.ingest.UpsertUserMetrics(t.Context(), req)
	if err != nil || write.AppliedPeriods != 2 {
		t.Fatalf("%v %v", write, err)
	}
	for _, mode := range []commonv1.TransferMode{1, 2} {
		got := read(mode, 2)
		if got.Period.IsCurrent || got.Navigation.Previous != nil || got.Navigation.Next != nil || !got.Period.Range.Start.AsTime().Equal(req.State.DataFrom.AsTime()) || !got.Period.Range.End.AsTime().Equal(req.State.DataTo.AsTime()) || got.Period.Bounds.Coverage != 2 || len(got.Period.Bounds.Gaps) != 1 {
			t.Fatal(got)
		}
		flat := screenBlock(read(mode, 1), "tiles").Data.AsMap()["tiles"].([]any)
		groups := screenBlock(got, "tiles").Data.AsMap()["groups"].([]any)
		primary := groups[0].(map[string]any)["tiles"].([]any)
		wantExpense := float64(35000)
		if mode == 2 {
			wantExpense = 30000
		}
		if primary[0].(map[string]any)["value"] != wantExpense || primary[1].(map[string]any)["value"] != float64(50000) || flat[1].(map[string]any)["value"] != wantExpense {
			t.Fatal(groups)
		}
		if a := primary[0].(map[string]any)["action"].(map[string]any)["filter"].(map[string]any); a["transfer_mode"] != map[commonv1.TransferMode]string{1: "with", 2: "without"}[mode] {
			t.Fatal(a)
		}
		points := screenBlock(got, "monthly_chart").Data.AsMap()["points"].([]any)
		if len(points) != 11 || points[1].(map[string]any)["value"] != float64(0) || points[1].(map[string]any)["action"].(map[string]any)["date"] != "2026-01" || points[2].(map[string]any)["action"] != nil {
			t.Fatal(points)
		}
		list, e := tr.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{Period: 4})
		if e != nil || len(list.Periods) != 1 || list.Periods[0].Key != "all_time" || list.Periods[0].Coverage != 2 {
			t.Fatalf("%v %v", list, e)
		}
	}
	other, err := tr.analytics.GetAnalytics(metadata.AppendToOutgoingContext(t.Context(), "test-user", bob), &analyticsv1.GetAnalyticsRequest{Period: 4})
	if err != nil || other.HasData {
		t.Fatalf("tenant %v %v", other, err)
	}
	// Replaying an older batch cannot replace either mode, gaps or typed actions.
	old := proto.Clone(req).(*analyticsv1.UpsertUserMetricsRequest)
	old.CalcVersion--
	old.Periods[0].Metrics[0].Metric.ValueMinor = proto.Int64(999)
	before := read(1, 2)
	if _, err = tr.ingest.UpsertUserMetrics(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(screenBlock(before, "tiles").Data.AsMap(), screenBlock(read(1, 2), "tiles").Data.AsMap()) {
		t.Fatal("stale replay changed facts")
	}
	removed := proto.Clone(req).(*analyticsv1.UpsertUserMetricsRequest)
	removed.CalcVersion++
	removed.Periods = nil
	removed.RemovedPeriods = []*analyticsv1.PeriodRef{{Period: 4, PeriodKey: "all_time"}}
	if _, err = tr.ingest.UpsertUserMetrics(t.Context(), removed); err != nil {
		t.Fatal(err)
	}
	if _, err = tr.ingest.UpsertUserMetrics(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if screenBlock(read(1, 2), "tiles").Status != "unavailable" {
		t.Fatal("removal resurrected")
	}
	for _, key := range []string{"2026", "2026-10", "ALL_TIME"} {
		_, e := tr.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 4, PeriodKey: &key})
		if status.Code(e) != codes.InvalidArgument {
			t.Fatal(e)
		}
	}
}

func TestIntegrationCalendarGroupsNegotiateForecastAndEmptyPeriod(t *testing.T) {
	tr := startTransport(t)
	req := analyticsFixture(t)
	if _, err := tr.ingest.UpsertUserMetrics(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "test-user", alice)
	read := func(key string, version int32) *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		r, e := tr.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: &key, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: version}}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	v1 := screenBlock(read("2026-09", 1), "tiles")
	v2 := screenBlock(read("2026-09", 2), "tiles")
	old := map[string]map[string]any{}
	for _, raw := range v1.Data.AsMap()["tiles"].([]any) {
		m := raw.(map[string]any)
		old[m["id"].(string)] = m
	}
	groups := v2.Data.AsMap()["groups"].([]any)
	primary := groups[0].(map[string]any)["tiles"].([]any)
	extra := groups[1].(map[string]any)["tiles"].([]any)
	if len(primary) != 3 || len(extra) != 2 || extra[1].(map[string]any)["id"] != "forecast" || extra[1].(map[string]any)["value"] != float64(10120000) {
		t.Fatal(groups)
	}
	for i, id := range []string{"expense", "income", "balance"} {
		m := primary[i].(map[string]any)
		if m["id"] != id || !reflect.DeepEqual(m["action"], old[id]["action"]) || m["value"] != old[id]["value"] {
			t.Fatal(m)
		}
	}
	b := screenBlock(read("2025-01", 2), "tiles")
	if b.Status != "none" || len(b.Data.AsMap()["groups"].([]any)) != 0 {
		t.Fatal(b)
	}
	for _, supports := range [][]*commonv1.BlockSupport{{nil}, {{Type: "tile_group", MaxVersion: 0}}} {
		_, err := tr.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Supports: supports})
		rest, ok := finerr.ToREST(err)
		if !ok || len(rest.Violations) != 1 || rest.Violations[0].Field != "supports" || rest.Violations[0].Reason != "INVALID_FORMAT" {
			t.Fatal(err)
		}
	}
	// Failed capability reads leave the stored facts and next valid response unchanged.
	if !proto.Equal(v1, screenBlock(read("2026-09", 1), "tiles")) {
		t.Fatal("invalid read mutated snapshot")
	}
}
