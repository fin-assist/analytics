package domain

import "time"

// Rolling reports whether the type is a coverage-anchored singleton.
func (t PeriodType) Rolling() bool { return t == PeriodRollingYear || t == PeriodRollingQuarter }

// RollingRange shifts the exclusive covered-day boundary by calendar months with day clamping.
// It resolves each midnight independently in the profile zone; it never uses elapsed 24-hour days.
func RollingRange(typ PeriodType, end *time.Time, zone *time.Location) (TimeRange, error) {
	if !typ.Rolling() || end == nil || zone == nil {
		return TimeRange{}, invalid("rolling coverage boundary")
	}
	local := end.In(zone)
	finish, exists := CivilDayStart(local.Year(), local.Month(), local.Day(), zone)
	if !exists || !finish.Equal(*end) {
		return TimeRange{}, invalid("rolling end is not a covered-day boundary")
	}
	months := 3
	if typ == PeriodRollingYear {
		months = 12
	}
	target := time.Date(local.Year(), local.Month()-time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	day := local.Day()
	if day > last {
		day = last
	}
	start, exists := CivilBoundary(target.Year(), target.Month(), day, zone)
	if !exists || start.Year() < 1 || finish.Year() > 9999 || !start.Before(finish) {
		return TimeRange{}, invalid("rolling range out of bounds")
	}
	return TimeRange{Start: &start, End: &finish}, nil
}
