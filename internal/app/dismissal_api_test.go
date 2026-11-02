package app_test

import (
	"os"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func dismissalRequest(t *testing.T) *analyticsv1.ApplyRegularPaymentDismissalRequest {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/dismissal.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.ApplyRegularPaymentDismissalRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPIApplyComposesSuppliedMetricAndMarksWithoutFinancialWrites(t *testing.T) {
	env, c := startAPI(t)
	ctx := apitest.User(t.Context(), alice)
	r := dismissalRequest(t)
	before := proto.Clone(r)
	got, err := c.analytics.ApplyRegularPaymentDismissal(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d := got.Blocks[0].Data.AsMap()
	if !proto.Equal(before, r) || got.Currency != "RUB" || got.TimeZone != "Europe/Moscow" || got.RequiredCalcVersion != 11 || len(got.Blocks) != 1 || d["value"] != float64(999999) || !strings.Contains(d["description"].(string), "17 списаний") || len(d["items"].([]any)) != 3 || d["items_more"].(map[string]any)["value"] != float64(90000) {
		t.Fatal(got)
	}
	var stateRows, periods, metricRows int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM user_data_state),(SELECT count(*) FROM period_snapshot),(SELECT count(*) FROM period_metric)`).Scan(&stateRows, &periods, &metricRows); err != nil || stateRows != 0 || periods != 0 || metricRows != 0 {
		t.Fatal("Apply created financial state", stateRows, periods, metricRows, err)
	}
	screen, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09")})
	if err != nil || screen.HasData || screen.Calculation.GetCalcVersion() != 0 || !screen.Calculation.GetRecalculating() {
		t.Fatal(screen, err)
	}
	// A retry carries a freshly computed metric, retaining the same original target.
	r.Projection.Metrics[0].Metric.ValueMinor = proto.Int64(333333)
	got, err = c.analytics.ApplyRegularPaymentDismissal(ctx, r)
	if err != nil || got.Blocks[0].Data.AsMap()["value"] != float64(333333) || got.RequiredCalcVersion != 11 {
		t.Fatal(got, err)
	}
	// NONE and LOCKED remain supplied outcomes even without any analytics financial baseline.
	for _, s := range []analyticsv1.MetricStatus{analyticsv1.MetricStatus_METRIC_STATUS_NONE, analyticsv1.MetricStatus_METRIC_STATUS_LOCKED} {
		m := r.Projection.Metrics[0].Metric
		m.Status = s
		m.ValueMinor = nil
		m.Details = nil
		m.Filters = nil
		if s == analyticsv1.MetricStatus_METRIC_STATUS_LOCKED {
			m.Basis = nil
			m.Lock = &analyticsv1.MetricLock{Reason: 1, Required: proto.Int32(2), Available: proto.Int32(0)}
		}
		got, err = c.analytics.ApplyRegularPaymentDismissal(ctx, r)
		if err != nil || got.Blocks[0].Status == "unavailable" {
			t.Fatal(got, err)
		}
	}
}

func TestAPIApplyRefusesWrongCallerBindingAndMalformedPayload(t *testing.T) {
	env, c := startAPI(t)
	r := dismissalRequest(t)
	ctx := t.Context()
	for _, caller := range []string{"bank-bff", "bank-statement-data", "assistant"} {
		_, err := c.analytics.ApplyRegularPaymentDismissal(apitest.Caller(ctx, caller), r)
		if caller == "bank-bff" {
			if apitest.Reason(err) != "INVALID_TOKEN" {
				t.Fatal(err)
			}
		} else {
			wantBare(t, err, codes.PermissionDenied)
		}
	}
	_, err := c.analytics.ApplyRegularPaymentDismissal(apitest.User(ctx, bob), r)
	wantBare(t, err, codes.InvalidArgument)
	cases := map[string]func(*analyticsv1.ApplyRegularPaymentDismissalRequest){
		"absent":      func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection = nil },
		"wrong month": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.Params.PeriodKey = "2026-08" },
		"wrong metric": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].MetricId = "tiles.expense"
		},
		"duplicate": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics = append(r.Projection.Metrics, r.Projection.Metrics[0])
		},
		"missing count": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Count = nil
		},
		"dismissed present": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.RegularPaymentId = r.Projection.Metrics[0].Metric.GetRegularPayments().Items[0].Id
		},
		"Local":           func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.Projection.TimeZone = "Local" },
		"negative target": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) { r.CalcVersion = -1 },
		"oversize": func(r *analyticsv1.ApplyRegularPaymentDismissalRequest) {
			r.Projection.Metrics[0].Metric.GetRegularPayments().Items[0].Title = strings.Repeat("x", 3*1024*1024)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := dismissalRequest(t)
			mutate(r)
			_, err := c.analytics.ApplyRegularPaymentDismissal(apitest.User(ctx, alice), r)
			wantBare(t, err, codes.InvalidArgument)
		})
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM recalculation_marker`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid payload wrote marker", count, err)
	}
	_, err = c.deletion.PurgeUserData(apitest.Caller(ctx, "user"), &userdeletionv1.PurgeUserDataRequest{UserId: alice})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.analytics.ApplyRegularPaymentDismissal(apitest.User(ctx, alice), r)
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
}

func TestAPIApplyCompositionFailureRollsBackMarker(t *testing.T) {
	env, c := startAPI(t)
	r := dismissalRequest(t)
	// Safe int64 transport, but not exactly representable by the Block Struct wire.
	r.Projection.Metrics[0].Metric.ValueMinor = proto.Int64(1 << 53)
	_, err := c.analytics.ApplyRegularPaymentDismissal(apitest.User(t.Context(), alice), r)
	rest, ok := finerr.ToREST(err)
	if !ok || rest.Code != "INTERNAL_ERROR" || rest.RequestRetryable == nil || !*rest.RequestRetryable {
		t.Fatal(err)
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalculation_marker`).Scan(&count); err != nil || count != 0 {
		t.Fatal("composition failure committed", count, err)
	}
	r.Projection.Metrics[0].Metric.ValueMinor = proto.Int64(7)
	if _, err = c.analytics.ApplyRegularPaymentDismissal(apitest.User(t.Context(), alice), r); err != nil {
		t.Fatal(err)
	}
}
