package metrics

import (
	"strconv"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

// snapshotLabelZone retains the calendar of a persisted snapshot while the profile's
// zone may already have changed. It changes text projection only, never a stored instant.
// The original IANA name is not in PeriodSnapshot: a mismatched calendar is represented
// by the offset of its known first midnight, not by an invented IANA zone.
func snapshotLabelZone(key domain.PeriodKey, r *domain.TimeRange, profile *time.Location) *time.Location {
	if r == nil || r.Start == nil || r.End == nil || key.Validate() != nil {
		return profile
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
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, profile)
	if start.Equal(*r.Start) && start.AddDate(0, months, 0).Equal(*r.End) {
		return profile
	}
	utc := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	offset := utc.Sub(*r.Start)
	if offset < -24*time.Hour || offset > 24*time.Hour {
		return profile
	}
	return time.FixedZone("snapshot-calendar", int(offset/time.Second))
}

func (p composition) calendar() *time.Location {
	if p.labels != nil {
		return p.labels
	}
	return p.zone
}

func (p composition) pointCalendar(point *analyticsv1.MonthlyPoint) *time.Location {
	r := point.GetRange()
	if r.GetStart() == nil || r.GetEnd() == nil {
		return p.calendar()
	}
	start, end := r.Start.AsTime(), r.End.AsTime()
	return snapshotLabelZone(domain.PeriodKey{Type: domain.PeriodMonth, Key: point.GetMonth()}, &domain.TimeRange{Start: &start, End: &end}, p.calendar())
}

// Basis/comparison bounds are calendar month midnights. A fixed snapshot offset may
// differ from a historical bound's offset because of DST. Taking the nearest month
// boundary recovers its calendar month without moving the financial timestamp itself.
func calendarMonth(ts *timestamppb.Timestamp, labels *time.Location) time.Time {
	t := ts.AsTime().In(labels)
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, labels)
	next := start.AddDate(0, 1, 0)
	if t.Sub(start) > next.Sub(t) {
		return next
	}
	return start
}

// Coverage starts and gap boundaries are calendar midnights, not operation times.
// Normalize a historical DST offset difference only for those known boundaries.
func calendarDay(t time.Time, labels *time.Location) time.Time {
	t = t.In(labels)
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, labels)
	next := start.AddDate(0, 0, 1)
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
