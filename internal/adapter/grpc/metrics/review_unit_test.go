package metrics

import (
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitForecastLabelKeepsSelectedKeyAcrossTimezoneRebuild(t *testing.T) {
	for _, tc := range []struct {
		typ              domain.PeriodType
		key, start, want string
	}{{domain.PeriodMonth, "2026-09", "2026-09-01T00:00:00+03:00", "на сентябрь"}, {domain.PeriodQuarter, "2026-Q4", "2026-10-01T00:00:00+03:00", "на IV квартал"}, {domain.PeriodYear, "2026", "2026-01-01T00:00:00+03:00", "на 2026 год"}} {
		s := screenFixture(t)
		s.Key.Type = tc.typ
		s.Key.Key = tc.key
		from, _ := time.Parse(time.RFC3339, tc.start)
		s.Range.Start = &from
		p := composition{s: s, zone: time.UTC}
		if got := p.forecastCaption(); got != tc.want {
			t.Errorf("%s: %s want %s", tc.key, got, tc.want)
		}
	}
}

func TestUnitBalanceSurvivesCorruptMonthlyDetails(t *testing.T) {
	s := screenFixture(t)
	fixtureMetric(&s, "monthly_chart").Details = []byte(`{"monthly_chart":"broken"}`)
	got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if screen := byID(got, "monthly_chart"); screen.Status != "unavailable" {
		t.Fatal(screen)
	}
	b := byID(got, "balance")
	if b.Status != "ready" {
		t.Fatalf("healthy balance hidden: %+v", b)
	}
	d := valueOf(b)
	if len(d["points"].([]any)) != 0 || d["summary"].(map[string]any)["value"] != float64(7258000) {
		t.Fatal(d)
	}
}

func TestUnitBalanceTextRoundsHalfUpAndDoesNotPrintNegativeZero(t *testing.T) {
	for _, tc := range []struct {
		share  float64
		prefix string
	}{{-0.004, "0%"}, {-0.005, "0%"}, {-0.015, "1%"}, {0.005, "1%"}} {
		s := screenFixture(t)
		setDetails(t, &s, "tiles.balance", &analyticsv1.Metric{Details: &analyticsv1.Metric_Balance{Balance: &analyticsv1.BalanceDetails{ShareOfIncome: &tc.share}}})
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		caption := valueOf(byID(got, "balance"))["caption"].(string)
		if !strings.HasPrefix(caption, tc.prefix) || strings.Contains(caption, "-0%") {
			t.Fatalf("%v: %s", tc.share, caption)
		}
		if tc.share < 0 && !strings.Contains(caption, "расходы по выписке больше доходов") {
			t.Fatal(caption)
		}
	}
}

func TestUnitHeadingRequiresSupportedObservationCards(t *testing.T) {
	got, err := composerForTest(t).Compose(t.Context(), screenFixture(t), []*commonv1.BlockSupport{{Type: "heading", MaxVersion: 1}, {Type: "tile_group", MaxVersion: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got.Blocks {
		if b.Type == "heading" {
			t.Fatal("orphan heading")
		}
	}
}

func TestUnitUnknownPointCoverageIsOmitted(t *testing.T) {
	for _, bounds := range []*commonv1.DataBounds{nil, {Coverage: 99}, {Coverage: 0}} {
		p := composition{zone: time.UTC}
		point := &analyticsv1.MonthlyPoint{Bounds: bounds}
		if got := p.barPoint(point, int64(0)); got["coverage"] != nil {
			t.Fatalf("unknown coverage invented: %+v", got)
		}
	}
}

func TestUnitSmallFrequentLargestGroupUsesCardActionOnly(t *testing.T) {
	s := screenFixture(t)
	original, err := metric(*fixtureMetric(&s, "insights.small_frequent"))
	if err != nil {
		t.Fatal(err)
	}
	largest := original.GetSmallFrequent().Items[0]
	second := &analyticsv1.SmallFrequentGroup{Title: "Прочие покупки", Count: 5, AverageAmountMinor: 20000, MonthlyAmountMinor: 100000, YearlyAmountMinor: 1200000, Filters: largest.Filters}
	original.GetSmallFrequent().Items = append(original.GetSmallFrequent().Items, second)
	setDetails(t, &s, "insights.small_frequent", &analyticsv1.Metric{Details: original.Details})
	fixtureMetric(&s, "insights.small_frequent").ValueMinor = ptr(int64(330000))
	got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := byID(got, "small_frequent")
	d := valueOf(b)
	items := d["items"].([]any)
	if b.GetAction().GetFilter().GetSelection() != "s_small_coffee" || len(items) != 1 || items[0].(map[string]any)["title"] != "Прочие покупки" {
		t.Fatalf("largest group repeated among rows: %+v", b)
	}
}
