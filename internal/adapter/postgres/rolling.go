package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

// rollingPeriods uses the same statement snapshot as GetAnalytics: an incomplete
// publication must not combine a new coverage anchor with an older singleton header.
func (r *Metrics) rollingPeriods(ctx context.Context, user string, typ domain.PeriodType) (domain.Periods, error) {
	key := typ.String()
	data, err := r.ReadAnalytics(ctx, user, typ, &key, domain.TransferWith)
	if err != nil {
		return domain.Periods{}, err
	}
	if data.State == nil {
		return domain.Periods{}, nil
	}
	state := data.State
	out := domain.Periods{TimeZone: state.TimeZone, Revisions: state.Revisions}
	if state.DataFrom == nil || state.DataTo == nil {
		return out, nil
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return domain.Periods{}, err
	}
	span, err := domain.RollingRange(typ, state.DataTo, zone)
	if err != nil {
		return domain.Periods{}, errors.New("analytics: invalid stored rolling boundary")
	}
	coverage := domain.CoveragePartial
	if snapshot := data.Snapshot; snapshot != nil && snapshot.Range != nil && snapshot.Range.Start != nil && snapshot.Range.End != nil && snapshot.Range.Start.Equal(*span.Start) && snapshot.Range.End.Equal(*span.End) {
		coverage = snapshot.Coverage
		out.Revisions = snapshot.Revisions.Intersect(state.Revisions)
	}
	out.Items = []domain.PeriodItem{{Key: key, Start: *span.Start, End: *span.End, Coverage: coverage}}
	return out, nil
}
