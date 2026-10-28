package metrics

import (
	"fmt"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

// detailsMarshal is the stored form of the details oneof: protojson of a Metric holding only
// it, the key naming the variant ({"categories": {"items": […]}}, {"bank_fees": {}}). Enum
// numbers, as in the columns; int64 as strings, without loss (docs/schema.md, «period_metric»).
var detailsMarshal = protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, args...))
}

// timeOf converts an optional timestamp; one outside the range of Timestamp is refused
// instead of being normalised into another moment.
func timeOf(ts *timestamppb.Timestamp) (*time.Time, error) {
	if ts == nil {
		return nil, nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil, invalid("timestamp out of range")
	}
	t := ts.AsTime()
	return &t, nil
}

func rangeOf(r *commonv1.TimeRange) (*domain.TimeRange, error) {
	if r == nil {
		return nil, nil
	}
	start, err := timeOf(r.GetStart())
	if err != nil {
		return nil, err
	}
	end, err := timeOf(r.GetEnd())
	if err != nil {
		return nil, err
	}
	return &domain.TimeRange{Start: start, End: end}, nil
}

// canonicalUserID turns a valid UUID of either case into its canonical lower-case form, the
// form of the tombstone lock and of every comparison of the service: two calls for one user in
// different cases must serialize on one guard and match one sub. Anything else is returned as is
// and refused by validation.
func canonicalUserID(id string) string {
	if !domain.ValidUserID(id) {
		return id
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return id
	}
	return parsed.String()
}

// upsertOf converts UpsertUserMetricsRequest into the domain batch. It refuses only what
// the domain cannot represent — a MetricSnapshot without its Metric, a filter with q or
// amounts (no columns for them, they would be lost silently), an invalid timestamp; the
// rules of the values are domain.Upsert.Validate.
func upsertOf(req *analyticsv1.UpsertUserMetricsRequest) (domain.Upsert, error) {
	u := domain.Upsert{UserID: canonicalUserID(req.GetUserId()), Version: req.GetCalcVersion()}
	if req.State != nil {
		s, err := stateOf(req.GetState())
		if err != nil {
			return domain.Upsert{}, err
		}
		u.State = &s
	}
	for _, p := range req.GetPeriods() {
		s, err := snapshotOf(p)
		if err != nil {
			return domain.Upsert{}, err
		}
		u.Snapshots = append(u.Snapshots, s)
	}
	for _, r := range req.GetRemovedPeriods() {
		u.Removed = append(u.Removed, domain.PeriodKey{Type: domain.PeriodType(r.GetPeriod()), Key: r.GetPeriodKey()})
	}
	return u, nil
}

func stateOf(s *analyticsv1.UserDataState) (domain.State, error) {
	out := domain.State{HasData: s.GetHasData(), FullMonths: s.GetFullMonths(), Recalculating: s.GetRecalculating(), TimeZone: s.GetTimeZone()}
	for _, f := range []struct {
		to   **time.Time
		from *timestamppb.Timestamp
	}{
		{&out.DataFrom, s.GetDataFrom()}, {&out.DataTo, s.GetDataTo()},
		{&out.LastOperationAt, s.GetLastOperationAt()}, {&out.StaleAfter, s.GetStaleAfter()},
		{&out.CalculatedAt, s.GetCalculatedAt()},
	} {
		t, err := timeOf(f.from)
		if err != nil {
			return domain.State{}, err
		}
		*f.to = t
	}
	var err error
	out.Revisions, err = revisionsOf(s.GetDataRevisions())
	if err != nil {
		return domain.State{}, err
	}
	return out, nil
}

// SnapshotOf converts one PeriodSnapshot. It is exported for the repository tests, which
// write through the same conversion the handler uses.
func SnapshotOf(p *analyticsv1.PeriodSnapshot) (domain.Snapshot, error) { return snapshotOf(p) }

func snapshotOf(p *analyticsv1.PeriodSnapshot) (domain.Snapshot, error) {
	s := domain.Snapshot{
		Key: domain.SnapshotKey{
			PeriodKey: domain.PeriodKey{Type: domain.PeriodType(p.GetPeriod()), Key: p.GetPeriodKey()},
			Mode:      domain.TransferMode(p.GetTransferMode()),
		},
		Coverage:    domain.Coverage(p.GetBounds().GetCoverage()),
		UnreadLines: p.GetUnreadLinesCount(),
	}
	var err error
	s.Revisions, err = revisionsOf(p.GetDataRevisions())
	if err != nil {
		return domain.Snapshot{}, err
	}
	if s.Range, err = rangeOf(p.GetRange()); err != nil {
		return domain.Snapshot{}, err
	}
	if s.DataFrom, err = timeOf(p.GetBounds().GetDataFrom()); err != nil {
		return domain.Snapshot{}, err
	}
	if s.DataTo, err = timeOf(p.GetBounds().GetDataTo()); err != nil {
		return domain.Snapshot{}, err
	}
	if s.CalculatedAt, err = timeOf(p.GetCalculatedAt()); err != nil {
		return domain.Snapshot{}, err
	}
	for _, g := range p.GetBounds().GetGaps() {
		r, err := rangeOf(g)
		if err != nil {
			return domain.Snapshot{}, err
		}
		if r == nil {
			r = &domain.TimeRange{}
		}
		s.Gaps = append(s.Gaps, *r)
	}
	for _, ms := range p.GetMetrics() {
		m, err := metricOf(ms)
		if err != nil {
			return domain.Snapshot{}, fmt.Errorf("snapshot %s: %w", p.GetPeriodKey(), err)
		}
		s.Metrics = append(s.Metrics, m)
	}
	return s, nil
}

func metricOf(ms *analyticsv1.MetricSnapshot) (domain.Metric, error) {
	m := ms.GetMetric()
	if m == nil {
		return domain.Metric{}, invalid("metric %s without value", ms.GetMetricId())
	}
	out := domain.Metric{
		ID: ms.GetMetricId(), DefinitionVersion: ms.GetDefinitionVersion(),
		Status: int32(m.GetStatus()), ValueMinor: m.ValueMinor,
	}
	var err error
	if out.Range, err = rangeOf(m.GetRange()); err != nil {
		return domain.Metric{}, err
	}
	if l := m.GetLock(); l != nil {
		// lock.text of the producer is ignored: analytics builds the text (grpc-api.md 7.3).
		from, err := timeOf(l.GetAvailableFrom())
		if err != nil {
			return domain.Metric{}, err
		}
		out.Lock = &domain.Lock{Reason: int32(l.GetReason()), Required: l.Required, Available: l.Available, AvailableFrom: from}
	}
	if b := m.GetBasis(); b != nil {
		r, err := rangeOf(b.GetRange())
		if err != nil {
			return domain.Metric{}, err
		}
		out.Basis = &domain.Basis{Range: r, FullMonths: b.GetFullMonths()}
	}
	if f := m.GetFilters(); f != nil {
		if f.Q != nil || f.AmountFromMinor != nil || f.AmountToMinor != nil {
			return domain.Metric{}, invalid("metric %s: filter with q or amounts", ms.GetMetricId())
		}
		from, err := timeOf(f.GetFrom())
		if err != nil {
			return domain.Metric{}, err
		}
		to, err := timeOf(f.GetTo())
		if err != nil {
			return domain.Metric{}, err
		}
		out.Filter = &domain.Filter{
			From: from, To: to, CategoryID: f.CategoryId,
			Kind: int32(f.GetKind()), TransferMode: int32(f.GetTransferMode()),
			Selection: f.Selection, SelectionName: f.SelectionName,
		}
	}
	if m.GetDetails() != nil {
		b, err := detailsMarshal.Marshal(&analyticsv1.Metric{Details: m.Details})
		if err != nil {
			return domain.Metric{}, invalid("metric %s: details", ms.GetMetricId())
		}
		out.Details = b
	}
	return out, nil
}
