package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

// GetAnalytics reads a screen for the verified subject in either admitted call model.
func (h *analytics) GetAnalytics(ctx context.Context, req *analyticsv1.GetAnalyticsRequest) (*analyticsv1.GetAnalyticsResponse, error) {
	user, ok := subject(ctx, req.UserId)
	if !ok || !domain.ValidUserID(user) {
		return nil, invalidArgument()
	}
	typ, mode := domain.PeriodType(req.GetPeriod()), domain.TransferMode(req.GetTransferMode())
	if typ == 0 {
		typ = domain.PeriodMonth
	}
	if mode == 0 {
		mode = domain.TransferWith
	}
	if !typ.Known() {
		return nil, validation("period")
	}
	if mode != domain.TransferWith && mode != domain.TransferWithout {
		return nil, validation("transfer_mode")
	}
	if h.screen == nil || h.composer == nil {
		return nil, errors.New("analytics: screen not wired")
	}
	screen, err := h.screen.Get(ctx, user, typ, req.PeriodKey, mode)
	if errors.Is(err, domain.ErrInvalid) {
		return nil, validation("date")
	}
	if err != nil {
		return nil, err
	}
	if screen.State == nil || !screen.State.HasData {
		var revisions domain.Revisions
		if screen.State != nil {
			revisions = screen.State.Revisions
		}
		return &analyticsv1.GetAnalyticsResponse{DataRevisions: revisionMessages(revisions)}, nil
	}
	response, err := h.composer.Compose(ctx, screen, req.Supports, req.Experiments)
	if err != nil {
		return nil, err
	}
	revisions := screen.State.Revisions
	if screen.Snapshot != nil {
		revisions = screen.Snapshot.Revisions.Intersect(screen.State.Revisions)
	}
	response.DataRevisions = revisionMessages(revisions)
	return response, nil
}

func validation(field string) error {
	return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: field, Description: "invalid analytics parameter"})
}

func timestamp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
func timerange(r *domain.TimeRange) *commonv1.TimeRange {
	if r == nil {
		return nil
	}
	return &commonv1.TimeRange{Start: timestamp(r.Start), End: timestamp(r.End)}
}
func filter(f *domain.Filter) *commonv1.OperationsFilter {
	if f == nil {
		return nil
	}
	return &commonv1.OperationsFilter{From: timestamp(f.From), To: timestamp(f.To), CategoryId: f.CategoryID,
		Kind: commonv1.KindFilter(f.Kind), TransferMode: commonv1.TransferMode(f.TransferMode), Selection: f.Selection, SelectionName: f.SelectionName}
}

func metric(m domain.Metric) (*analyticsv1.Metric, error) {
	out := &analyticsv1.Metric{Status: analyticsv1.MetricStatus(m.Status), ValueMinor: m.ValueMinor, Range: timerange(m.Range), Filters: filter(m.Filter)}
	if m.Lock != nil {
		l := m.Lock
		out.Lock = &analyticsv1.MetricLock{Reason: analyticsv1.LockReason(l.Reason), Required: l.Required, Available: l.Available, AvailableFrom: timestamp(l.AvailableFrom)}
	}
	if m.Basis != nil {
		out.Basis = &analyticsv1.MetricBasis{Range: timerange(m.Basis.Range), FullMonths: m.Basis.FullMonths}
	}
	if len(m.Details) > 0 {
		holder := &analyticsv1.Metric{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Details, holder); err != nil {
			return nil, fmt.Errorf("metric details: %w", err)
		}
		out.Details = holder.Details
	}
	if balance := out.GetBalance(); balance != nil && balance.ShareOfIncome != nil && (math.IsNaN(*balance.ShareOfIncome) || math.IsInf(*balance.ShareOfIncome, 0)) {
		return nil, errors.New("non-finite balance share")
	}
	return out, nil
}
