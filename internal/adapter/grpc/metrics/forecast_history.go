package metrics

import (
	"strings"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

const forecastHistoryScope = ". База прогноза — вся загруженная история, независимо от выбранного года."
const forecastHistoryRule = " Полный месяц — завершённый календарный месяц с данными за каждый день."

func (p composition) forecastHistoryText(lock *analyticsv1.MetricLock) string {
	history := p.s.History
	if history == nil || lock.Available == nil || !p.preciseDays() {
		return forecastHistoryScope + forecastHistoryRule
	}
	full, incomplete, missing := []string{}, []string{}, []string{}
	byKey := map[string]bool{}
	for _, month := range history.Months {
		byKey[month.Key] = true
		name := periodName(domain.PeriodKey{Type: domain.PeriodMonth, Key: month.Key})
		if month.Coverage == domain.CoverageComplete {
			full = append(full, month.Key)
			continue
		}
		reasons := []string{"данные за " + dayRange(month.DataFrom.In(p.zone), month.DataTo.In(p.zone))}
		if month.Range.End.After(*p.s.State.CalculatedAt) {
			reasons = append(reasons, "месяц ещё не закончился")
		}
		for _, gap := range month.Gaps {
			reasons = append(reasons, "нет данных за "+dayRange(gap.Start.In(p.zone), gap.End.Add(-time.Nanosecond).In(p.zone)))
		}
		incomplete = append(incomplete, name+" ("+strings.Join(reasons, "; ")+")")
	}
	if len(history.Months) > 0 {
		first := history.Months[0].Range.Start.In(p.zone)
		last := history.Months[len(history.Months)-1].Range.Start.In(p.zone)
		for month := first; month.Before(last); month = month.AddDate(0, 1, 0) {
			key := month.Format("2006-01")
			if !byKey[key] {
				missing = append(missing, key)
			}
		}
	}
	if int32(len(full)) != lock.GetAvailable() {
		return forecastHistoryScope + forecastHistoryRule
	}
	fullText := "нет"
	if len(full) != 0 {
		fullText = historyMonthList(full)
	}
	result := forecastHistoryScope + forecastHistoryRule + " Учтены полные месяцы: " + fullText + "."
	if len(incomplete) != 0 {
		result += " Неполные месяцы: " + strings.Join(incomplete, ", ") + "."
	}
	if len(missing) != 0 {
		result += " Месяцы без данных: " + historyMonthList(missing) + "."
	}
	return result
}

// Consecutive calendar months share one range; disconnected bases keep separate labels.
func historyMonthList(keys []string) string {
	groups := []string{}
	for first := 0; first < len(keys); {
		last := first
		for last+1 < len(keys) {
			month, _ := time.Parse("2006-01", keys[last])
			if month.AddDate(0, 1, 0).Format("2006-01") != keys[last+1] {
				break
			}
			last++
		}
		label := periodName(domain.PeriodKey{Type: domain.PeriodMonth, Key: keys[first]})
		if last > first {
			label += " — " + periodName(domain.PeriodKey{Type: domain.PeriodMonth, Key: keys[last]})
		}
		groups = append(groups, label)
		first = last + 1
	}
	return strings.Join(groups, ", ")
}
