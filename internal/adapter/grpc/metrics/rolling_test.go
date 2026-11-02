package metrics

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/domain"
)

func rollingFixture(t *testing.T) *analyticsv1.UpsertUserMetricsRequest {
	t.Helper()
	raw, err := os.ReadFile("../../../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	return req
}

func rollingNavigation(t *testing.T, zone string) *domain.ChartPage {
	t.Helper()
	points := []*analyticsv1.MonthlyPoint{}
	for i := range 13 {
		key := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0).Format("2006-01")
		points = append(points, navigationPoint(t, zone, key, commonv1.Coverage_COVERAGE_COMPLETE, ptr(int64(100000+i))))
	}
	return navigationFixture(t, zone, points...)
}

func TestUnitRollingComposerKeepsSelectedFactsAndFullHistoryNavigation(t *testing.T) {
	req := rollingFixture(t)
	converted, err := upsertOf(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := converted.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, snap := range converted.Snapshots {
		t.Run(fmt.Sprint(snap.Key.Type.String(), snap.Key.Mode), func(t *testing.T) {
			snap.TimeZone = converted.State.TimeZone
			s := domain.Analytics{AnalyticsData: domain.AnalyticsData{Chart: rollingNavigation(t, converted.State.TimeZone), State: converted.State, Snapshot: &snap}, Key: snap.Key, Range: *snap.Range}
			got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Blocks) != 4 || got.Period.IsCurrent || got.Navigation.Previous != nil || got.Navigation.Next != nil {
				t.Fatalf("header %v", got)
			}
			tiles := valueOf(byID(got, "tiles"))["tiles"].([]any)
			if len(tiles) != 4 {
				t.Fatalf("forecast leaked: %v", tiles)
			}
			if caption := tiles[2].(map[string]any)["caption"]; caption != "по покрытым дням" {
				t.Fatalf("comparison leaked %v", caption)
			}
			chart := valueOf(byID(got, "monthly_chart"))
			if _, ok := chart["highlight_index"]; ok {
				t.Fatal("singleton highlighted")
			}
			if len(chart["notes"].([]any)) != 1 {
				t.Fatalf("typical/comparison leaked %v", chart["notes"])
			}
			points := chart["points"].([]any)
			want := 13
			if len(points) != want {
				t.Fatalf("points %d", len(points))
			}
			if snap.Key.Type == domain.PeriodRollingYear && (!strings.Contains(byID(got, "monthly_chart").GetAltText(), "октябрь 2025") || !strings.Contains(byID(got, "monthly_chart").GetAltText(), "октябрь 2026")) {
				t.Fatal("edge month years ambiguous")
			}
			first := points[0].(map[string]any)
			if first["range"].(map[string]any)["from"] != "2025-10-01T00:00:00+03:00" || first["range"].(map[string]any)["to"] != "2025-11-01T00:00:00+03:00" || first["value"] != float64(100000) {
				t.Fatalf("navigation did not use full calendar history: %v", first)
			}
			if first["action"].(map[string]any)["date"] != "2025-10" || !proto.Equal(got.Period.Range, timerange(snap.Range)) {
				t.Fatal("navigation changed selected rolling facts")
			}
			for _, raw := range points {
				point := raw.(map[string]any)
				if point["coverage"] == "no_data" {
					if _, ok := point["action"]; ok {
						t.Fatal("gap actionable")
					}
					if value, ok := point["value"]; ok && value != nil {
						t.Fatal("gap became zero")
					}
				}
			}
			// Typed details survive the exact JSON representation used by persistence.
			stored := snap.Metrics[len(snap.Metrics)-1]
			decoded, err := metric(stored)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(decoded.GetMonthlyChart(), req.Periods[indexOfSnapshot(req, snap.Key)].Metrics[len(snap.Metrics)-1].Metric.GetMonthlyChart()) {
				t.Fatal("calculation_range lost")
			}
		})
	}
}

func indexOfSnapshot(req *analyticsv1.UpsertUserMetricsRequest, key domain.SnapshotKey) int {
	for i, p := range req.Periods {
		if int32(p.Period) == int32(key.Type) && int32(p.TransferMode) == int32(key.Mode) {
			return i
		}
	}
	return -1
}

func TestUnitRollingMissingAndMalformedDeliveryNeverInventsFacts(t *testing.T) {
	req := rollingFixture(t)
	u, err := upsertOf(req)
	if err != nil {
		t.Fatal(err)
	}
	s := domain.Analytics{AnalyticsData: domain.AnalyticsData{State: u.State}, Key: u.Snapshots[0].Key, Range: *u.Snapshots[0].Range}
	got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Period.Bounds.Coverage != commonv1.Coverage_COVERAGE_PARTIAL || byID(got, "tiles").Status != "unavailable" || got.Period.Bounds.DataFrom != nil {
		t.Fatalf("fabricated missing period %v", got)
	}
	s.Snapshot = &u.Snapshots[0]
	s.Snapshot.TimeZone = u.State.TimeZone
	s.Chart = rollingNavigation(t, u.State.TimeZone)
	holder, err := metric(s.Snapshot.Metrics[len(s.Snapshot.Metrics)-1])
	if err != nil {
		t.Fatal(err)
	}
	original := proto.Clone(holder).(*analyticsv1.Metric)
	for _, change := range []string{"absent", "outside", "month", "timestamp", "coverage", "unknown_zero", "edge_complete", "covered_nil"} {
		holder = proto.Clone(original).(*analyticsv1.Metric)
		point := holder.GetMonthlyChart().Points[0]
		switch change {
		case "absent":
			point.CalculationRange = nil
		case "outside":
			point.CalculationRange = proto.Clone(point.Range).(*commonv1.TimeRange)
		case "month":
			point.Month = "2026-06"
		case "timestamp":
			point.CalculationRange.Start.Seconds = 1 << 62
		case "coverage":
			point.Bounds.Coverage = commonv1.Coverage(99)
		case "unknown_zero":
			point.Bounds.Coverage = commonv1.Coverage_COVERAGE_NO_DATA
		case "edge_complete":
			point.Bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
		case "covered_nil":
			point.ExpenseMinor = nil
		}
		bytes, err := detailsMarshal.Marshal(holder)
		if err != nil {
			if change == "timestamp" {
				continue
			}
			t.Fatal(err)
		}
		s.Snapshot.Metrics[len(s.Snapshot.Metrics)-1].Details = bytes
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if byID(got, "monthly_chart").Status != "ready" || valueOf(byID(got, "monthly_chart"))["notes"] != nil || byID(got, "tiles").Status != "ready" {
			t.Fatalf("malformed %s escaped isolation: %v", change, got)
		}
	}
}
