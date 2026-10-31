package domain

import "time"

// CivilDayStart returns the earliest real instant belonging to an IANA civil date.
// A midnight gap starts later than 00:00; a repeated midnight uses its first occurrence.
// A skipped entire date has no start. The search intersects only the zone intervals
// in a bounded UTC window around the civil date, rather than normalizing time.Date midnight.
func CivilDayStart(year int, month time.Month, day int, zone *time.Location) (time.Time, bool) {
	if zone == nil || year < 1 || year > 9999 {
		return time.Time{}, false
	}
	civil := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if civil.Year() != year || civil.Month() != month || civil.Day() != day {
		return time.Time{}, false
	}
	next := civil.AddDate(0, 0, 1)
	limit := civil.Add(48 * time.Hour)
	var earliest time.Time
	found := false
	for probe := civil.Add(-48 * time.Hour); probe.Before(limit); {
		local := probe.In(zone)
		_, offset := local.Zone()
		begin, end := local.ZoneBounds()
		first, last := civil.Add(-time.Duration(offset)*time.Second), next.Add(-time.Duration(offset)*time.Second)
		if !begin.IsZero() && first.Before(begin) {
			first = begin
		}
		if !end.IsZero() && last.After(end) {
			last = end
		}
		if first.Before(last) && (!found || first.Before(earliest)) {
			earliest = first
			found = true
		}
		if end.IsZero() || !end.After(probe) {
			break
		}
		probe = end
	}
	return earliest.In(zone), found
}

// CivilBoundary resolves a skipped civil date to the first following real date,
// as a local date's start-of-day boundary. It never advances by elapsed 24-hour periods.
func CivilBoundary(year int, month time.Month, day int, zone *time.Location) (time.Time, bool) {
	civil := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if zone == nil || year < 1 || year > 9999 || civil.Year() != year || civil.Month() != month || civil.Day() != day {
		return time.Time{}, false
	}
	for range 3 {
		if start, ok := CivilDayStart(civil.Year(), civil.Month(), civil.Day(), zone); ok {
			return start, true
		}
		civil = civil.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}
