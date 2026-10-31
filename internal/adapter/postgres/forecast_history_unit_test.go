package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func historyFixture(t *testing.T) ([]historyRow, domain.State, *domain.Snapshot) {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2025, 10, 30, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, zone)
	now := to.Add(-12 * time.Hour)
	version := int32(4)
	revisions := domain.Revisions{"categories": 0, "uploads": 2, "time_zone": 0}
	envelope := json.RawMessage(`{"calc_version":4,"revisions":{"categories":0,"uploads":2,"time_zone":0}}`)
	stale := to.AddDate(0, 0, 14)
	state := domain.State{HasData: true, LastOperationAt: &from, StaleAfter: &stale, DataFrom: &from, DataTo: &to, CalculatedAt: &now, FullMonths: 11, TimeZone: zone.String(), Revisions: revisions}
	selected := &domain.Snapshot{Revisions: revisions}
	rows := []historyRow{}
	for month := time.Date(2025, 10, 1, 0, 0, 0, 0, zone); month.Before(to); month = month.AddDate(0, 1, 0) {
		start, end := month, month.AddDate(0, 1, 0)
		dataFrom, dataTo := start, end.Add(-time.Second)
		coverage := domain.CoverageComplete
		if month.Month() == time.October {
			coverage = domain.CoveragePartial
			if month.Year() == 2025 {
				dataFrom = from
			} else {
				dataTo = to.Add(-time.Second)
			}
		}
		rows = append(rows, historyRow{HistoryMonth: domain.HistoryMonth{Key: month.Format("2006-01"), Range: domain.TimeRange{Start: &start, End: &end}, Coverage: coverage, DataFrom: &dataFrom, DataTo: &dataTo}, Version: &version, Revisions: envelope})
	}
	return rows, state, selected
}

func TestUnitForecastHistoryUsesCompletedInventoryAcrossCalcVersions(t *testing.T) {
	rows, state, selected := historyFixture(t)
	rows[1].Version = new(int32(3))
	rows[1].Revisions = json.RawMessage(`{"calc_version":3,"revisions":{"categories":0,"uploads":2,"time_zone":0}}`)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	got := forecastHistory(raw, state, selected)
	if got == nil || len(got.Months) != 13 || got.Months[1].Key != "2025-11" || got.Months[12].Coverage != domain.CoveragePartial {
		t.Fatalf("history %+v", got)
	}
}

func TestUnitForecastHistoryRefusesUnprovenOrInconsistentInventory(t *testing.T) {
	for _, name := range []string{"legacy", "selected revision", "recalculating", "missing full", "count", "envelope version", "month revision", "timezone", "duplicate", "order", "nil range", "bounds", "unknown coverage", "complete future", "gap", "global first", "global last", "invalid json"} {
		t.Run(name, func(t *testing.T) {
			rows, state, selected := historyFixture(t)
			switch name {
			case "legacy":
				state.Revisions = nil
			case "selected revision":
				selected.Revisions = domain.Revisions{"categories": 0, "uploads": 1, "time_zone": 0}
			case "recalculating":
				state.Recalculating = true
			case "missing full":
				rows = append(rows[:1], rows[2:]...)
			case "count":
				state.FullMonths++
			case "envelope version":
				rows[1].Version = new(int32(5))
			case "month revision":
				rows[1].Revisions = json.RawMessage(`{"calc_version":4,"revisions":{"categories":0,"uploads":1,"time_zone":0}}`)
			case "timezone":
				state.TimeZone = "America/New_York"
			case "duplicate":
				rows[1] = rows[0]
			case "order":
				rows[1], rows[2] = rows[2], rows[1]
			case "nil range":
				rows[1].Range.Start = nil
			case "bounds":
				rows[1].DataFrom = rows[1].Range.End
			case "unknown coverage":
				rows[1].Coverage = 99
			case "complete future":
				rows[12].Coverage = domain.CoverageComplete
				state.FullMonths++
			case "gap":
				rows[1].Gaps = []domain.TimeRange{{}}
			case "global first":
				value := state.DataFrom.AddDate(0, 0, 1)
				state.DataFrom = &value
			case "global last":
				value := state.DataTo.AddDate(0, 0, 1)
				state.DataTo = &value
			}
			raw, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if name == "invalid json" {
				raw = []byte(`[`)
			}
			if got := forecastHistory(raw, state, selected); got != nil {
				t.Fatalf("unproven history %+v", got)
			}
		})
	}
}

func TestUnitForecastHistoryDailyRolloverNormalizesRetainedPartialMonth(t *testing.T) {
	rows, state, selected := historyFixture(t)
	end := *rows[12].Range.End
	last := end.Add(-time.Second)
	now := end.Add(12 * time.Hour)
	rows[12].DataTo = &last
	state.DataTo = &end
	state.CalculatedAt = &now
	state.FullMonths = 12
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	got := forecastHistory(raw, state, selected)
	if got == nil || got.Months[12].Coverage != domain.CoverageComplete {
		t.Fatalf("daily rollover retained a partial month: %+v", got)
	}
}

func TestUnitForecastHistoryCertifiedInventoryDoesNotRequireSelectedData(t *testing.T) {
	rows, state, _ := historyFixture(t)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if got := forecastHistory(raw, state, nil); got == nil || len(got.Months) != 13 {
		t.Fatalf("known no-data history %+v", got)
	}
	state.Revisions = nil
	if forecastHistory(raw, state, nil) != nil {
		t.Fatal("legacy no-data fabricated a coverage inventory")
	}
}
