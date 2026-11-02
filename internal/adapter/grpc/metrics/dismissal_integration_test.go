package metrics_test

import (
	"os"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestIntegrationDismissalSuppliedPayloadAndMarkerOnRealTransport(t *testing.T) {
	e := startTransport(t)
	raw, err := os.ReadFile("../../../../testdata/dismissal.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.ApplyRegularPaymentDismissalRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "test-user", alice)
	out, err := e.analytics.ApplyRegularPaymentDismissal(ctx, r)
	if err != nil || out.GetRequiredCalcVersion() != 11 || out.Blocks[0].Data.AsMap()["value"] != float64(999999) {
		t.Fatal(out, err)
	}
	var version int32
	if err = e.db.Primary().QueryRow(t.Context(), `SELECT requested_version FROM recalculation_marker WHERE user_id=$1`, alice).Scan(&version); err != nil || version != 11 {
		t.Fatal(version, err)
	}
	for _, state := range []analyticsv1.MetricStatus{analyticsv1.MetricStatus_METRIC_STATUS_NONE, analyticsv1.MetricStatus_METRIC_STATUS_LOCKED} {
		copy := proto.Clone(r).(*analyticsv1.ApplyRegularPaymentDismissalRequest)
		m := copy.Projection.Metrics[0].Metric
		m.Status = state
		m.ValueMinor = nil
		m.Details = nil
		m.Filters = nil
		if state == analyticsv1.MetricStatus_METRIC_STATUS_LOCKED {
			m.Basis = nil
			m.Lock = &analyticsv1.MetricLock{Reason: 1, Required: proto.Int32(2), Available: proto.Int32(0)}
		}
		if out, err = e.analytics.ApplyRegularPaymentDismissal(ctx, copy); err != nil || len(out.Blocks) != 1 {
			t.Fatal(out, err)
		}
	}
	r.Projection.Metrics[0].Metric.ValueMinor = nil
	_, err = e.analytics.ApplyRegularPaymentDismissal(ctx, r)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	if err = e.db.Primary().QueryRow(t.Context(), `SELECT requested_version FROM recalculation_marker WHERE user_id=$1`, alice).Scan(&version); err != nil || version != 11 {
		t.Fatal(version, err)
	}
}
