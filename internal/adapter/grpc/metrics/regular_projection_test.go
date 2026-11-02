package metrics

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

func dismissalFixture(t *testing.T) *analyticsv1.ApplyRegularPaymentDismissalRequest {
	t.Helper()
	raw, err := os.ReadFile("../../../../testdata/dismissal.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.ApplyRegularPaymentDismissalRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	return r
}

type fakeChanges struct {
	calls int
	err   error
}

func (f *fakeChanges) Mark(context.Context, string, int32) error { f.calls++; return f.err }
func (f *fakeChanges) Apply(ctx context.Context, _ string, _ int32, fn func(context.Context) error) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return fn(ctx)
}

func TestUnitApplyUsesSuppliedValuesWithoutGlobalState(t *testing.T) {
	req := dismissalFixture(t)
	before := proto.Clone(req)
	uc := &fakeChanges{}
	h := &analytics{changes: uc}
	got, err := h.ApplyRegularPaymentDismissal(reqctx.WithUserID(t.Context(), req.Projection.UserId), req)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(req, before) {
		t.Fatal("composition mutated caller payload")
	}
	b := got.Blocks[0]
	d := b.Data.AsMap()
	if len(got.Blocks) != 1 || got.TimeZone != "Europe/Moscow" || got.RequiredCalcVersion != 11 || d["value"] != float64(999999) || !strings.Contains(d["description"].(string), "17 списаний") || len(d["items"].([]any)) != 3 || d["items_more"].(map[string]any)["value"] != float64(90000) {
		t.Fatalf("supplied projection lost: %v", got)
	}
	if b.Action.GetFilter().GetSelection() != "s_regular_all" {
		t.Fatal(b)
	}
	for _, state := range []analyticsv1.MetricStatus{analyticsv1.MetricStatus_METRIC_STATUS_NONE, analyticsv1.MetricStatus_METRIC_STATUS_LOCKED} {
		m := req.Projection.Metrics[0].Metric
		m.Status = state
		m.ValueMinor = nil
		m.Details = nil
		m.Filters = nil
		if state == analyticsv1.MetricStatus_METRIC_STATUS_LOCKED {
			m.Basis = nil
			m.Lock = &analyticsv1.MetricLock{Reason: 1, Required: proto.Int32(2), Available: proto.Int32(0)}
		}
		got, err = h.ApplyRegularPaymentDismissal(reqctx.WithUserID(t.Context(), req.Projection.UserId), req)
		if err != nil || len(got.Blocks) != 1 || got.Blocks[0].Status == "unavailable" {
			t.Fatalf("empty/locked %v %v", got, err)
		}
	}
}

func TestUnitApplyRejectsMalformedProjectionBeforeEffects(t *testing.T) {
	cases := map[string]func(*analyticsv1.ApplyRegularPaymentDismissalRequest){
		"projection": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection = nil },
		"target":     func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.CalcVersion = 0 },
		"id":         func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.RegularPaymentId = "bad" },
		"subject":    func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.UserId = r.RegularPaymentId },
		"params": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Params = proto.Clone(r.Params).(*commonv1.AnalyticsParams)
			r.Params.PeriodKey = "2026-08"
		},
		"mode": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Params.TransferMode = 0
			r.Projection.Params.TransferMode = 0
		},
		"currency": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Currency = "USD" },
		"zone":     func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.TimeZone = "Local" },
		"metrics":  func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Metrics = nil },
		"duplicate": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics = append(r.Projection.Metrics, r.Projection.Metrics[0])
		},
		"metric id": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].MetricId = "tiles.expense"
		},
		"definition": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].DefinitionVersion = 0
		},
		"value": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.ValueMinor = nil
		},
		"count": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Count = nil
		},
		"details": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Metrics[0].Metric.Details = nil },
		"empty items": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Items = nil
		},
		"item duplicate": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			d := r.Projection.Metrics[0].Metric.GetRegularPayments()
			d.Items[1] = d.Items[0]
		},
		"dismissed": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.RegularPaymentId = r.Projection.Metrics[0].Metric.GetRegularPayments().Items[0].Id
		},
		"range": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Range.End = r.Projection.Range.Start
		},
		"source range": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.DataRange = nil },
		"bounds":       func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Bounds = nil },
		"timestamp": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Bounds.DataTo = &timestamppb.Timestamp{Seconds: 253402300800}
		},
		"coverage": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Bounds.Coverage = 0 },
		"selection": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Items[0].Filters.Selection = proto.String("other")
		},
		"oversize": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Items[0].Title = strings.Repeat("x", maxProjectionBytes)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := dismissalFixture(t)
			user := r.Projection.UserId
			change(r)
			uc := &fakeChanges{}
			h := &analytics{changes: uc}
			_, err := h.ApplyRegularPaymentDismissal(reqctx.WithUserID(t.Context(), user), r)
			if status.Code(err) != codes.InvalidArgument || len(status.Convert(err).Details()) != 0 || uc.calls != 0 {
				t.Fatalf("refusal/effects: %v %d", err, uc.calls)
			}
		})
	}
}

func TestUnitApplyCancelledCompositionDoesNotSucceed(t *testing.T) {
	r := dismissalFixture(t)
	ctx, cancel := context.WithCancel(reqctx.WithUserID(t.Context(), r.Projection.UserId))
	cancel()
	_, err := (&analytics{changes: &fakeChanges{}}).ApplyRegularPaymentDismissal(ctx, r)
	if err == nil {
		t.Fatal("cancelled composition succeeded")
	}
	if _, e := regularProjectionOf(nil, ""); !errors.Is(e, domain.ErrInvalid) {
		t.Fatal(e)
	}
}

func TestUnitApplyIgnoresUnknownFieldsWithoutWeakeningBinding(t *testing.T) {
	r := dismissalFixture(t)
	r.Projection.Params.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	r.Projection.Range.Start.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	uc := &fakeChanges{}
	h := &analytics{changes: uc}
	ctx := reqctx.WithUserID(t.Context(), r.Projection.UserId)
	if _, err := h.ApplyRegularPaymentDismissal(ctx, r); err != nil || uc.calls != 1 {
		t.Fatalf("unknown fields rejected: %v", err)
	}
	r.Projection.Params.TransferMode = commonv1.TransferMode_TRANSFER_MODE_WITHOUT
	if _, err := h.ApplyRegularPaymentDismissal(ctx, r); status.Code(err) != codes.InvalidArgument || uc.calls != 1 {
		t.Fatalf("known binding ignored: %v", err)
	}
}
