package metrics

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

// AnalyticsStore reads one screen in one database statement, including the default key.
type AnalyticsStore interface {
	ReadAnalytics(context.Context, string, domain.PeriodType, *string, domain.TransferMode) (domain.AnalyticsData, error)
}

// Screen resolves the selected calendar period and current-time flags without calculating metrics.
type Screen struct {
	store AnalyticsStore
	now   func() time.Time
}

// NewScreen wires the screen usecase. Both dependencies are required.
func NewScreen(store AnalyticsStore, now func() time.Time) (*Screen, error) {
	if store == nil || now == nil {
		return nil, errors.New("analytics: dependencies required")
	}
	return &Screen{store: store, now: now}, nil
}

// Get reads only the authenticated user's data. An omitted key chooses the newest covered period.
func (s *Screen) Get(ctx context.Context, user string, typ domain.PeriodType, key *string, mode domain.TransferMode) (domain.Analytics, error) {
	if !domain.ValidUserID(user) || !typ.Known() || (mode != domain.TransferWith && mode != domain.TransferWithout) {
		return domain.Analytics{}, fmt.Errorf("%w: analytics parameters", domain.ErrInvalid)
	}
	if key != nil {
		exact, err := exactKey(typ, *key)
		if err != nil {
			return domain.Analytics{}, err
		}
		key = &exact
		if typ != domain.PeriodAllTime {
			if _, _, err := calendarRange(typ, exact, time.UTC); err != nil {
				return domain.Analytics{}, err
			}
		}
	}
	if typ == domain.PeriodAllTime {
		// A single non-calendar key also works before the first new producer snapshot arrives.
		exact := "all_time"
		key = &exact
	}
	explicit := key != nil
	data, err := s.store.ReadAnalytics(ctx, user, typ, key, mode)
	if err != nil {
		return domain.Analytics{}, err
	}
	out := domain.Analytics{AnalyticsData: data}
	if data.Snapshot == nil {
		// calc.md section 4: an explicit period outside the covered list has no neighbours.
		out.Previous, out.Next = nil, nil
	}
	if data.State == nil || !data.State.HasData {
		return out, nil
	}
	if err := data.State.Validate(); err != nil {
		return domain.Analytics{}, errors.New("analytics: invalid stored state")
	}
	if key == nil {
		key = data.SelectedKey
	}
	if key == nil {
		return domain.Analytics{}, errors.New("analytics: data state without covered periods")
	}
	zone, err := time.LoadLocation(data.State.TimeZone)
	if err != nil {
		return domain.Analytics{}, err
	}
	var start, end time.Time
	if typ == domain.PeriodAllTime {
		start, end = *data.State.DataFrom, *data.State.DataTo
		out.Previous, out.Next = nil, nil
	} else {
		start, end, err = calendarRange(typ, *key, zone)
		if err != nil {
			if !explicit {
				return domain.Analytics{}, errors.New("analytics: invalid stored period")
			}
			return domain.Analytics{}, err
		}
	}
	out.Key = domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: typ, Key: *key}, Mode: mode}
	out.Range = domain.TimeRange{Start: &start, End: &end}
	// An old period snapshot keeps its original calendar bounds while a time-zone rebuild runs.
	if data.Snapshot != nil {
		out.Range = *data.Snapshot.Range
	}
	now := s.now()
	calculated := data.State.CalculatedAt.In(zone)
	current := now.In(zone)
	out.Rollover = calculated.Year() != current.Year() || calculated.Month() != current.Month()
	if out.Rollover {
		// A previous month's completion count cannot explain today's calendar until daily delivery finishes.
		out.History = nil
	}
	out.Current = typ != domain.PeriodAllTime && !now.Before(*out.Range.Start) && now.Before(*out.Range.End)
	out.Stale = data.State.StaleAfter != nil && !now.Before(*data.State.StaleAfter)
	return out, nil
}

func exactKey(typ domain.PeriodType, key string) (string, error) {
	if typ == domain.PeriodAllTime {
		if err := (domain.PeriodKey{Type: typ, Key: key}).Validate(); err != nil {
			return "", err
		}
		return key, nil
	}
	for _, input := range []domain.PeriodType{domain.PeriodMonth, domain.PeriodQuarter, domain.PeriodYear} {
		if (domain.PeriodKey{Type: input, Key: key}).Validate() != nil {
			continue
		}
		if input < typ {
			year := key[:4]
			if typ == domain.PeriodYear {
				return year, nil
			}
			month, _ := strconv.Atoi(key[5:])
			return fmt.Sprintf("%s-Q%d", year, (month-1)/3+1), nil
		}
		if input == typ {
			return key, nil
		}
		break
	}
	return "", fmt.Errorf("%w: period key", domain.ErrInvalid)
}

func calendarRange(typ domain.PeriodType, key string, zone *time.Location) (time.Time, time.Time, error) {
	if err := (domain.PeriodKey{Type: typ, Key: key}).Validate(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	year, _ := strconv.Atoi(key[:4])
	month, months := 1, 12
	if typ == domain.PeriodMonth {
		month, _ = strconv.Atoi(key[5:])
		months = 1
	}
	if typ == domain.PeriodQuarter {
		quarter, _ := strconv.Atoi(key[6:])
		month = (quarter-1)*3 + 1
		months = 3
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, zone)
	end := start.AddDate(0, months, 0)
	if start.Year() < 1 || end.Year() > 9999 {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: period range", domain.ErrInvalid)
	}
	return start, end, nil
}
