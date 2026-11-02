package metrics

import (
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

const maxProjectionBytes = 3 * 1024 * 1024
const regularMetricID = "insights.regular_payments"

// regularProjectionOf validates only the supplied regular block context, not a fabricated global state.
func regularProjectionOf(req *analyticsv1.ApplyRegularPaymentDismissalRequest, user string) (composition, error) {
	p := req.GetProjection()
	if p == nil || proto.Size(p) > maxProjectionBytes || req.GetCalcVersion() <= 0 || !canonicalID(req.GetRegularPaymentId()) || !canonicalID(p.GetUserId()) || p.UserId != user {
		return composition{}, domain.ErrInvalid
	}
	key := domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodType(req.GetParams().GetPeriod()), Key: req.GetParams().GetPeriodKey()}, Mode: domain.TransferMode(req.GetParams().GetTransferMode())}
	if key.Type != domain.PeriodMonth || key.Validate() != nil || (key.Mode != domain.TransferWith && key.Mode != domain.TransferWithout) || !sameProjectionParams(req.GetParams(), p.GetParams()) || p.Currency != "RUB" || domain.ValidateSourceTimeZone(p.TimeZone) != nil {
		return composition{}, domain.ErrInvalid
	}
	zone, _ := time.LoadLocation(p.TimeZone)
	r, err := projectionRange(p.Range)
	if err != nil || !profileCalendarMatches(key.PeriodKey, r, zone) {
		return composition{}, domain.ErrInvalid
	}
	source, err := projectionRange(p.DataRange)
	if err != nil {
		return composition{}, err
	}
	snap, err := projectionBounds(p.Bounds, key, zone)
	if err != nil {
		return composition{}, err
	}
	if len(p.Metrics) != 1 || p.Metrics[0].GetMetricId() != regularMetricID || p.Metrics[0].GetDefinitionVersion() <= 0 {
		return composition{}, domain.ErrInvalid
	}
	if err = validateRegularMetric(p.Metrics[0].GetMetric(), req.GetRegularPaymentId(), p.Params, p.Range); err != nil {
		return composition{}, err
	}
	m, err := metricOf(p.Metrics[0])
	if err != nil {
		return composition{}, err
	}
	snap.Key, snap.Range, snap.TimeZone = key, r, p.TimeZone
	snap.Metrics = []domain.Metric{m}
	return composition{s: domain.Analytics{AnalyticsData: domain.AnalyticsData{Snapshot: snap}, Key: key, Range: *r}, zone: zone, metrics: map[string]domain.Metric{regularMetricID: m}, sourceRange: source}, nil
}

func canonicalID(id string) bool { return domain.ValidUserID(id) && canonicalUserID(id) == id }

func projectionRange(wire *commonv1.TimeRange) (*domain.TimeRange, error) {
	r, err := rangeOf(wire)
	if err != nil || r == nil || r.Start == nil || r.End == nil || !r.Start.Before(*r.End) {
		return nil, domain.ErrInvalid
	}
	return r, nil
}

func projectionBounds(b *commonv1.DataBounds, key domain.SnapshotKey, zone *time.Location) (*domain.Snapshot, error) {
	if b == nil || (b.Coverage != commonv1.Coverage_COVERAGE_COMPLETE && b.Coverage != commonv1.Coverage_COVERAGE_PARTIAL) {
		return nil, domain.ErrInvalid
	}
	from, err := timeOf(b.DataFrom)
	if err != nil || from == nil {
		return nil, domain.ErrInvalid
	}
	to, err := timeOf(b.DataTo)
	if err != nil || to == nil || to.Before(*from) {
		return nil, domain.ErrInvalid
	}
	// Membership is by source civil day: a historical midnight rollback can put data_to after range.end.
	if from.In(zone).Format("2006-01") != key.Key || to.In(zone).Format("2006-01") != key.Key {
		return nil, domain.ErrInvalid
	}
	local := from.In(zone)
	start, exists := domain.CivilDayStart(local.Year(), local.Month(), local.Day(), zone)
	if !exists || !start.Equal(*from) {
		return nil, domain.ErrInvalid
	}
	s := &domain.Snapshot{Coverage: domain.Coverage(b.Coverage), DataFrom: from, DataTo: to}
	for _, gap := range b.Gaps {
		g, e := projectionRange(gap)
		if e != nil {
			return nil, e
		}
		s.Gaps = append(s.Gaps, *g)
	}
	return s, nil
}

func validateRegularMetric(m *analyticsv1.Metric, dismissed string, params *commonv1.AnalyticsParams, r *commonv1.TimeRange) error {
	if m == nil || m.Range != nil {
		return domain.ErrInvalid
	}
	if m.Basis != nil {
		if _, err := projectionRange(m.Basis.Range); err != nil || m.Basis.FullMonths < 0 {
			return domain.ErrInvalid
		}
	}
	switch m.Status {
	case analyticsv1.MetricStatus_METRIC_STATUS_READY, analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE:
		return validateRegularItems(m, dismissed, params, r)
	case analyticsv1.MetricStatus_METRIC_STATUS_LOCKED:
		if m.Lock == nil || m.Lock.Reason == 0 || m.Basis != nil || m.ValueMinor != nil || m.Details != nil || m.Filters != nil {
			return domain.ErrInvalid
		}
		if (m.Lock.Required != nil && m.Lock.GetRequired() < 0) || (m.Lock.Available != nil && m.Lock.GetAvailable() < 0) {
			return domain.ErrInvalid
		}
		_, err := timeOf(m.Lock.AvailableFrom)
		return err
	case analyticsv1.MetricStatus_METRIC_STATUS_NONE:
		if m.Lock != nil || m.ValueMinor != nil || m.Details != nil || m.Filters != nil {
			return domain.ErrInvalid
		}
		return nil
	default:
		return domain.ErrInvalid
	}
}

func validateRegularItems(m *analyticsv1.Metric, dismissed string, params *commonv1.AnalyticsParams, r *commonv1.TimeRange) error {
	d := m.GetRegularPayments()
	if m.Lock != nil || m.ValueMinor == nil || m.Basis == nil || d == nil || d.Count == nil || d.GetCount() < 0 || len(d.Items) == 0 || !regularFilter(m.Filters, params, r, "s_regular_all") {
		return domain.ErrInvalid
	}
	ids := make(map[string]bool, len(d.Items))
	for _, item := range d.Items {
		if item == nil || !canonicalID(item.Id) || ids[item.Id] || item.Id == dismissed || !regularFilter(item.Filters, params, r, "s_regular_"+item.Id) {
			return domain.ErrInvalid
		}
		ids[item.Id] = true
	}
	return nil
}

func regularFilter(f *commonv1.OperationsFilter, params *commonv1.AnalyticsParams, r *commonv1.TimeRange, selection string) bool {
	return f != nil && f.Q == nil && f.AmountFromMinor == nil && f.AmountToMinor == nil && f.Kind == commonv1.KindFilter_KIND_FILTER_EXPENSE && f.TransferMode == params.TransferMode && f.GetSelection() == selection && sameProjectionTimestamp(f.From, r.Start) && sameProjectionTimestamp(f.To, r.End)
}

// Compare understood semantics; future protobuf fields do not invalidate the binding.
func sameProjectionParams(a, b *commonv1.AnalyticsParams) bool {
	return a != nil && b != nil && a.Period == b.Period && a.PeriodKey == b.PeriodKey && a.TransferMode == b.TransferMode
}
func sameProjectionTimestamp(a, b *timestamppb.Timestamp) bool {
	return a.CheckValid() == nil && b.CheckValid() == nil && a.Seconds == b.Seconds && a.Nanos == b.Nanos
}
