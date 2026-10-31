package metrics

import (
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

// Only the producer's current, settled profile-day bounds certify all-time day labels.
func (p composition) preciseAllTimeDays() bool {
	s := p.s
	if s.State == nil || s.State.Recalculating || s.State.DataFrom == nil || s.State.DataTo == nil ||
		s.Range.Start == nil || s.Range.End == nil || p.zone == nil {
		return false
	}
	if !s.Range.Start.Equal(*s.State.DataFrom) || !s.Range.End.Equal(*s.State.DataTo) {
		return false
	}
	for _, bound := range []*time.Time{s.Range.Start, s.Range.End} {
		local := bound.In(p.zone)
		if local.Hour() != 0 || local.Minute() != 0 || local.Second() != 0 || local.Nanosecond() != 0 {
			return false
		}
	}
	return true
}

func (p composition) chartMonthName(key string, names [12]string) string {
	name := monthName(key, names)
	if p.s.Key.Type == domain.PeriodAllTime && (domain.PeriodKey{Type: domain.PeriodMonth, Key: key}).Validate() == nil {
		name += " " + key[:4]
	}
	return name
}
