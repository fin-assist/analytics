package metrics

import (
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitFactTileSurvivesLockedForecastAndPastPeriod(t *testing.T) {
	for _, current := range []bool{true, false} {
		s := screenFixture(t)
		s.Current = current
		f := fixtureMetric(&s, "tiles.forecast")
		f.Status = int32(analyticsv1.MetricStatus_METRIC_STATUS_LOCKED)
		f.ValueMinor = nil
		f.Lock = &domain.Lock{Reason: 1, Required: ptr(int32(12)), Available: ptr(int32(11))}
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tiles := valueOf(byID(got, "tiles"))["tiles"].([]any)
		fact := tiles[1].(map[string]any)
		if fact["id"] != "expense" || fact["value"] != float64(8432000) || fact["action"] == nil {
			t.Fatal(fact)
		}
		if current {
			if len(tiles) != 5 || tiles[4].(map[string]any)["status"] != "locked" {
				t.Fatal(tiles)
			}
			text := tiles[4].(map[string]any)["lock"].(map[string]any)["text"].(string)
			if !strings.Contains(text, "независимо от выбранного года") || !strings.Contains(text, "данными за каждый день") {
				t.Fatal(text)
			}
		} else if len(tiles) != 4 {
			t.Fatal(tiles)
		}
	}
}

func TestUnitForecastHistoryListsNonAdjacentMonthsAndPartialReasons(t *testing.T) {
	s := screenFixture(t)
	zone, _ := time.LoadLocation(s.State.TimeZone)
	month := func(key string, full bool) domain.HistoryMonth {
		start, _ := time.ParseInLocation("2006-01", key, zone)
		end := start.AddDate(0, 1, 0)
		last := end.Add(-time.Second)
		coverage := domain.CoveragePartial
		if full {
			coverage = domain.CoverageComplete
		}
		return domain.HistoryMonth{Key: key, Range: domain.TimeRange{Start: &start, End: &end}, DataFrom: &start, DataTo: &last, Coverage: coverage}
	}
	partial := month("2026-09", false)
	last := partial.Range.End.AddDate(0, 0, -5)
	partial.DataTo = &last
	gapStart := partial.Range.Start.AddDate(0, 0, 10)
	gapEnd := gapStart.AddDate(0, 0, 3)
	partial.Gaps = []domain.TimeRange{{Start: &gapStart, End: &gapEnd}}
	s.History = &domain.ForecastHistory{Months: []domain.HistoryMonth{month("2025-11", true), month("2026-02", true), partial}}
	p := composition{s: s, zone: zone}
	text := p.forecastHistoryText(&analyticsv1.MetricLock{Available: ptr(int32(2))})
	for _, want := range []string{"ноябрь 2025, февраль 2026", "сентябрь 2026 (данные за", "месяц ещё не закончился", "нет данных за 11–13 сентября", "Месяцы без данных: декабрь 2025 — январь 2026"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "ноябрь 2025 — февраль") {
		t.Fatal(text)
	}
	if got := p.forecastHistoryText(&analyticsv1.MetricLock{Available: ptr(int32(3))}); got != forecastHistoryScope+forecastHistoryRule {
		t.Fatal(got)
	}
	s.History = &domain.ForecastHistory{Months: []domain.HistoryMonth{partial}}
	p.s = s
	if got := p.forecastHistoryText(&analyticsv1.MetricLock{Available: ptr(int32(0))}); !strings.Contains(got, "Учтены полные месяцы: нет.") {
		t.Fatal(got)
	}
	if got := p.forecastHistoryText(&analyticsv1.MetricLock{}); got != forecastHistoryScope+forecastHistoryRule {
		t.Fatal("unknown available became zero", got)
	}
}

func TestUnitForecastHistoryCompressesOnlyConsecutiveMonths(t *testing.T) {
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"2025-11", "2025-12", "2026-01", "2026-03"}, "ноябрь 2025 — январь 2026, март 2026"},
		{[]string{"2020-01", "2026-09", "2026-10"}, "январь 2020, сентябрь 2026 — октябрь 2026"},
		{nil, ""},
	} {
		if got := historyMonthList(tc.keys); got != tc.want {
			t.Fatalf("%v: %s", tc.keys, got)
		}
	}
	keys := []string{}
	for month := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC); month.Before(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); month = month.AddDate(0, 1, 0) {
		keys = append(keys, month.Format("2006-01"))
	}
	if got := historyMonthList(keys); got != "январь 2020 — август 2026" {
		t.Fatal(got)
	}
}

func TestUnitKnownEarlyInventoryHidesChartForExplicitNoDataPeriod(t *testing.T) {
	s := screenFixture(t)
	s.Snapshot = nil
	s.History = &domain.ForecastHistory{Months: []domain.HistoryMonth{{Key: "2026-09", Coverage: domain.CoveragePartial}}}
	got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if byID(got, "monthly_chart") != nil {
		t.Fatal("global early chart fabricated for no-data period")
	}
	s.History = nil
	got, err = composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if byID(got, "monthly_chart") == nil || byID(got, "monthly_chart").Status != "unavailable" {
		t.Fatal("legacy unknown coverage was treated as proven early")
	}
}
