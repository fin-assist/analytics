package metrics

import (
	"errors"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

type periodSelection interface {
	defaultKey() *string
	exact(string) (string, error)
	validateKey(string) error
	bounds(string, *domain.State, *time.Location) (time.Time, time.Time, error)
	current(time.Time, domain.TimeRange) bool
	retain(*domain.TimeRange, domain.TimeRange) bool
}

type calendarSelection struct{ typ domain.PeriodType }

func (c calendarSelection) defaultKey() *string              { return nil }
func (c calendarSelection) exact(key string) (string, error) { return exactKey(c.typ, key) }
func (c calendarSelection) validateKey(key string) error {
	_, _, err := calendarRange(c.typ, key, time.UTC)
	return err
}
func (c calendarSelection) bounds(key string, _ *domain.State, zone *time.Location) (time.Time, time.Time, error) {
	return calendarRange(c.typ, key, zone)
}
func (c calendarSelection) current(now time.Time, r domain.TimeRange) bool {
	return !now.Before(*r.Start) && now.Before(*r.End)
}
func (c calendarSelection) retain(_ *domain.TimeRange, _ domain.TimeRange) bool { return true }

// allTimeSelection keeps persisted facts in their original coverage envelope during rebuilds.
type allTimeSelection struct{}

func (allTimeSelection) defaultKey() *string { key := "all_time"; return &key }
func (allTimeSelection) exact(key string) (string, error) {
	return key, (domain.PeriodKey{Type: domain.PeriodAllTime, Key: key}).Validate()
}
func (allTimeSelection) validateKey(key string) error {
	return (domain.PeriodKey{Type: domain.PeriodAllTime, Key: key}).Validate()
}
func (allTimeSelection) bounds(_ string, state *domain.State, _ *time.Location) (time.Time, time.Time, error) {
	return *state.DataFrom, *state.DataTo, nil
}
func (allTimeSelection) current(time.Time, domain.TimeRange) bool        { return false }
func (allTimeSelection) retain(*domain.TimeRange, domain.TimeRange) bool { return true }

type rollingSelection struct{ typ domain.PeriodType }

func (r rollingSelection) defaultKey() *string { key := r.typ.String(); return &key }
func (r rollingSelection) exact(key string) (string, error) {
	err := (domain.PeriodKey{Type: r.typ, Key: key}).Validate()
	return key, err
}
func (r rollingSelection) validateKey(key string) error {
	return (domain.PeriodKey{Type: r.typ, Key: key}).Validate()
}
func (r rollingSelection) bounds(_ string, state *domain.State, zone *time.Location) (time.Time, time.Time, error) {
	span, err := domain.RollingRange(r.typ, state.DataTo, zone)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("analytics: invalid stored rolling boundary")
	}
	return *span.Start, *span.End, nil
}
func (r rollingSelection) current(time.Time, domain.TimeRange) bool { return false }
func (r rollingSelection) retain(stored *domain.TimeRange, expected domain.TimeRange) bool {
	return stored != nil && stored.Start != nil && stored.End != nil && stored.Start.Equal(*expected.Start) && stored.End.Equal(*expected.End)
}

func selectionFor(typ domain.PeriodType) periodSelection {
	if typ == domain.PeriodAllTime {
		return allTimeSelection{}
	}
	if typ.Rolling() {
		return rollingSelection{typ: typ}
	}
	return calendarSelection{typ: typ}
}
