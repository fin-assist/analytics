package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

func navigationFixture(t *testing.T, zoneName string, points ...*analyticsv1.MonthlyPoint) *domain.ChartPage {
	t.Helper()
	raw, err := detailsMarshal.Marshal(&analyticsv1.Metric{Details: &analyticsv1.Metric_MonthlyChart{MonthlyChart: &analyticsv1.MonthlyChart{Points: points}}})
	if err != nil {
		t.Fatal(err)
	}
	return &domain.ChartPage{Details: raw, TimeZone: zoneName, Revision: "source-revision", Previous: "previous", Next: "next"}
}

func navigationPoint(t *testing.T, zoneName, key string, cov commonv1.Coverage, value *int64) *analyticsv1.MonthlyPoint {
	t.Helper()
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		t.Fatal(err)
	}
	start, end, ok := periodCalendarBounds(domain.PeriodKey{Type: domain.PeriodMonth, Key: key}, zone)
	if !ok {
		t.Fatal("invalid fixture calendar")
	}
	return &analyticsv1.MonthlyPoint{Month: key, Range: timerange(&domain.TimeRange{Start: &start, End: &end}), ExpenseMinor: value,
		Bounds: &commonv1.DataBounds{Coverage: cov}}
}

func TestUnitNavigationPageKeepsCoveredZeroHolesAndExactMonthActions(t *testing.T) {
	points := []*analyticsv1.MonthlyPoint{
		navigationPoint(t, "Europe/Moscow", "2025-12", commonv1.Coverage_COVERAGE_COMPLETE, ptr(int64(0))),
		navigationPoint(t, "Europe/Moscow", "2026-01", commonv1.Coverage_COVERAGE_NO_DATA, nil),
		navigationPoint(t, "Europe/Moscow", "2026-02", commonv1.Coverage_COVERAGE_PARTIAL, ptr(int64(12500))),
	}
	page := navigationFixture(t, "Europe/Moscow", points...)
	block, err := navigationChart(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	d := valueOf(block)
	bars := d["points"].([]any)
	if len(bars) != 3 || bars[0].(map[string]any)["value"] != float64(0) || bars[0].(map[string]any)["action"].(map[string]any)["date"] != "2025-12" {
		t.Fatal(bars)
	}
	hole := bars[1].(map[string]any)
	if hole["coverage"] != "no_data" || hole["value"] != nil || hole["action"] != nil {
		t.Fatal(hole)
	}
	if bars[2].(map[string]any)["action"].(map[string]any)["date"] != "2026-02" {
		t.Fatal(bars[2])
	}
	if d["notes"] != nil || d["highlight_index"] != nil || !strings.Contains(block.GetHelpText(), "всей загруженной истории") {
		t.Fatal(block)
	}
	meta := d["page"].(map[string]any)
	if meta["revision"] != "source-revision" || meta["previous"] != "previous" || meta["next"] != "next" {
		t.Fatal(meta)
	}
	// One covered month is still navigable even without two months of selected-period history.
	page = navigationFixture(t, "Europe/Moscow", points[0])
	block, err = navigationChart(t.Context(), page)
	if err != nil || block.Status != "ready" || len(valueOf(block)["points"].([]any)) != 1 {
		t.Fatalf("%v %v", block, err)
	}
}

func TestUnitNavigationPageUsesOwnHistoricalZoneForWireAndDayLabels(t *testing.T) {
	point := navigationPoint(t, "Europe/Moscow", "1900-01", commonv1.Coverage_COVERAGE_PARTIAL, ptr(int64(150)))
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := point.Range.Start.AsTime()
	to := time.Date(1900, 1, 15, 0, 15, 0, 0, zone)
	point.Bounds.DataFrom = timestamp(&from)
	point.Bounds.DataTo = timestamp(&to)
	block, err := navigationChart(t.Context(), navigationFixture(t, "Europe/Moscow", point))
	if err != nil {
		t.Fatal(err)
	}
	bar := valueOf(block)["points"].([]any)[0].(map[string]any)
	wire := bar["data_to"].(string)
	decoded, err := time.Parse(time.RFC3339, wire)
	if err != nil || !decoded.Equal(to) || !strings.HasSuffix(wire, "Z") {
		t.Fatalf("wire=%s %v", wire, err)
	}
	// UTC is the preceding civil day, but the label must follow the supplied IANA zone.
	if decoded.In(time.UTC).Day() != 14 || decoded.In(zone).Day() != 15 || bar["note"] != "по 15-е" {
		t.Fatal(bar)
	}
}

func TestUnitNavigationPageBoundsAndCancellation(t *testing.T) {
	point := navigationPoint(t, "Europe/Moscow", "2026-10", commonv1.Coverage_COVERAGE_COMPLETE, ptr(int64(0)))
	points := make([]*analyticsv1.MonthlyPoint, 61)
	for i := range points {
		points[i] = point
	}
	for _, page := range []*domain.ChartPage{navigationFixture(t, "Europe/Moscow"), navigationFixture(t, "Europe/Moscow", points...), navigationFixture(t, "Bad/Zone", point), {Details: []byte("{"), TimeZone: "UTC", Revision: "r"}} {
		if _, err := navigationChart(t.Context(), page); err == nil {
			t.Fatal("invalid page accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := navigationChart(ctx, navigationFixture(t, "Europe/Moscow", point)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnitNavigationPageRefusesMismatchedCalendarAndInventedZero(t *testing.T) {
	for _, change := range []string{"range", "coverage", "hole-zero", "covered-absent", "zone"} {
		t.Run(change, func(t *testing.T) {
			point := navigationPoint(t, "Europe/Moscow", "2026-10", commonv1.Coverage_COVERAGE_COMPLETE, ptr(int64(0)))
			page := navigationFixture(t, "Europe/Moscow", point)
			switch change {
			case "range":
				point.Range.Start.Seconds++
			case "coverage":
				point.Bounds.Coverage = 99
			case "hole-zero":
				point.Bounds.Coverage = commonv1.Coverage_COVERAGE_NO_DATA
			case "covered-absent":
				point.ExpenseMinor = nil
			case "zone":
				page.TimeZone = ""
			}
			if change != "zone" {
				page = navigationFixture(t, "Europe/Moscow", point)
			}
			if _, err := navigationChart(t.Context(), page); err == nil {
				t.Fatal("invalid point accepted")
			}
		})
	}
}
