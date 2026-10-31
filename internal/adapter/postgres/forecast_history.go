package postgres

import (
	"encoding/json"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

type historyRow struct {
	domain.HistoryMonth
	Version   *int32
	Revisions json.RawMessage
}

// A completed inventory with matching source revisions certifies missing months as no data.
// Legacy or partially delivered snapshots only support the general calendar-month explanation.
func forecastHistory(raw []byte, state domain.State, selected *domain.Snapshot) *domain.ForecastHistory {
	if state.Recalculating || (selected != nil && !historyRevisionsMatch(state.Revisions, selected.Revisions)) {
		return nil
	}
	var rows []historyRow
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 {
		return nil
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil || state.DataFrom == nil || state.DataTo == nil || state.CalculatedAt == nil {
		return nil
	}
	history := &domain.ForecastHistory{}
	full := int32(0)
	previous := ""
	for _, row := range rows {
		revisions, err := revisionsFromJSON(row.Revisions, row.Version)
		if err != nil || !historyRevisionsMatch(state.Revisions, revisions) || row.Key <= previous {
			return nil
		}
		month, err := time.ParseInLocation("2006-01", row.Key, zone)
		if err != nil || row.Range.Start == nil || row.Range.End == nil || !month.Equal(*row.Range.Start) || !month.AddDate(0, 1, 0).Equal(*row.Range.End) {
			return nil
		}
		if row.DataFrom == nil || row.DataTo == nil || row.DataFrom.After(*row.DataTo) || row.DataFrom.Before(month) || !row.DataTo.Before(*row.Range.End) {
			return nil
		}
		if row.Coverage != domain.CoverageComplete && row.Coverage != domain.CoveragePartial {
			return nil
		}
		// Daily recalculation may finish a former current month without replacing its header.
		// Its exact covered days remain authoritative; the fresh global count is the final check.
		complete := !row.Range.End.After(*state.CalculatedAt) && len(row.Gaps) == 0 &&
			row.DataFrom.Equal(*row.Range.Start) && nextCalendarDay(*row.DataTo, zone).Equal(*row.Range.End)
		row.Coverage = domain.CoveragePartial
		if complete {
			row.Coverage = domain.CoverageComplete
			full++
		}
		for _, gap := range row.Gaps {
			if gap.Start == nil || gap.End == nil || !gap.Start.Before(*gap.End) {
				return nil
			}
		}
		previous = row.Key
		history.Months = append(history.Months, row.HistoryMonth)
	}
	first, last := history.Months[0], history.Months[len(history.Months)-1]
	// Bounds plus completion proof distinguish a complete inventory from a retained subset.
	if full != state.FullMonths || !first.DataFrom.Equal(*state.DataFrom) || !nextCalendarDay(*last.DataTo, zone).Equal(*state.DataTo) {
		return nil
	}
	return history
}

func historyRevisionsMatch(state, snapshot domain.Revisions) bool {
	for _, code := range []string{"categories", "uploads", "time_zone"} {
		if _, ok := state[code]; !ok {
			return false
		}
	}
	for code, value := range state {
		if other, ok := snapshot[code]; !ok || other != value {
			return false
		}
	}
	return true
}

func nextCalendarDay(value time.Time, zone *time.Location) time.Time {
	day := value.In(zone)
	return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, zone)
}
