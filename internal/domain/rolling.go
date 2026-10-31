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

// SameBounds compares exact persisted instants; absent boundaries are never certified.
func (r *TimeRange) SameBounds(other *TimeRange) bool {
	return r != nil && other != nil && r.Start != nil && r.End != nil && other.Start != nil && other.End != nil && r.Start.Equal(*other.Start) && r.End.Equal(*other.End)
}

// RollingPeriods resolves list metadata from one coherent state/header read.
// It shares the rolling bounds and exact snapshot retention rule with screen selection.
func RollingPeriods(typ PeriodType, state *State, snapshots ...*Snapshot) (Periods, error) {
	if state == nil {
		return Periods{}, nil
	}
	out := Periods{TimeZone: state.TimeZone, Revisions: state.Revisions}
	if state.DataFrom == nil || state.DataTo == nil {
		return out, nil
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return Periods{}, err
	}
	span, err := RollingRange(typ, state.DataTo, zone)
	if err != nil {
		return Periods{}, err
	}
	coverage := CoveragePartial
	for _, snapshot := range snapshots {
		if snapshot == nil || !span.SameBounds(snapshot.Range) {
			continue
		}
		coverage = snapshot.Coverage
		out.Revisions = snapshot.Revisions.Intersect(state.Revisions)
		break
	}
	out.Items = []PeriodItem{{Key: typ.String(), Start: *span.Start, End: *span.End, Coverage: coverage}}
	return out, nil
}
