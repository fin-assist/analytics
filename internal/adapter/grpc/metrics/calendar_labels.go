package metrics

import (
	"fmt"
	"strconv"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

// periodCalendarBounds describes the civil period named by a key. It is used only
// to check metadata and label a period; stored financial bounds are never replaced.
func periodCalendarBounds(key domain.PeriodKey, zone *time.Location) (time.Time, time.Time, bool) {
	if zone == nil || key.Validate() != nil {
		return time.Time{}, time.Time{}, false
	}
	year, _ := strconv.Atoi(key.Key[:4])
	month, months := 1, 12
	switch key.Type {
	case domain.PeriodMonth:
		month, _ = strconv.Atoi(key.Key[5:])
		months = 1
	case domain.PeriodQuarter:
		quarter, _ := strconv.Atoi(key.Key[6:])
		month = (quarter-1)*3 + 1
		months = 3
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, zone)
	return start, start.AddDate(0, months, 0), true
}

func profileCalendarMatches(key domain.PeriodKey, r *domain.TimeRange, profile *time.Location) bool {
	if r == nil || r.Start == nil || r.End == nil {
		return false
	}
	start, end, ok := periodCalendarBounds(key, profile)
	return ok && start.Equal(*r.Start) && end.Equal(*r.End)
}

// preciseDays trusts the current profile calendar only for a settled snapshot whose
// boundaries agree with it. Rebuilding can retain a former IANA calendar even when
// both endpoint offsets happen to match, so matching offsets alone is insufficient.
func (p composition) preciseDays() bool {
	return p.s.State != nil && !p.s.State.Recalculating && profileCalendarMatches(p.s.Key.PeriodKey, &p.s.Range, p.zone)
}

func (p composition) precisePointDays(point *analyticsv1.MonthlyPoint) bool {
	r := point.GetRange()
	if !p.preciseDays() || r.GetStart() == nil || r.GetEnd() == nil {
		return false
	}
	start, end := r.Start.AsTime(), r.End.AsTime()
	return profileCalendarMatches(domain.PeriodKey{Type: domain.PeriodMonth, Key: point.GetMonth()}, &domain.TimeRange{Start: &start, End: &end}, p.zone)
}

func periodName(key domain.PeriodKey) string {
	start, _, ok := periodCalendarBounds(key, time.UTC)
	if !ok {
		return "выбранный период"
	}
	switch key.Type {
	case domain.PeriodMonth:
		return fmt.Sprintf("%s %d", monthNom[start.Month()-1], start.Year())
	case domain.PeriodQuarter:
		return fmt.Sprintf("%s квартал %d", [...]string{"I", "II", "III", "IV"}[(start.Month()-1)/3], start.Year())
	default:
		return fmt.Sprintf("%d год", start.Year())
	}
}

// Basis/comparison endpoints are declared full-calendar-month midnights. Their
// calendar month is the nearest UTC month boundary (a zone offset is far shorter
// than half a month). This does not recover IANA or apply to arbitrary operation times.
func calendarMonth(ts *timestamppb.Timestamp) time.Time {
	t := ts.AsTime().UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := start.AddDate(0, 1, 0)
	if t.Sub(start) > next.Sub(t) {
		return next
	}
	return start
}

func monthName(key string, names [12]string) string {
	month, err := time.Parse("2006-01", key)
	if err != nil {
		return key
	}
	return names[month.Month()-1]
}
