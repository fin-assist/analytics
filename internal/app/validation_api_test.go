package app_test

import (
	"net/http"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIAnalyticsPeriodValidationRESTDetails(t *testing.T) {
	_, clients := startAPI(t)
	client := clients.analytics
	ctx := apitest.User(t.Context(), alice)
	if _, err := clients.ingest.UpsertUserMetrics(producer(t.Context()), analyticsFixture(t)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		period commonv1.PeriodType
		key    string
	}{
		{"year to month", 1, "2026"}, {"year to quarter", 2, "2026"}, {"quarter to month", 1, "2026-Q3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: tc.period, PeriodKey: proto.String(tc.key)})
			mapped, ok := finerr.ToREST(err)
			if status.Code(err) != codes.InvalidArgument || !ok || mapped.Spec.HTTPStatus != http.StatusBadRequest || mapped.Code != "VALIDATION_ERROR" || len(mapped.Violations) != 1 || mapped.Violations[0].Field != "date" || mapped.Violations[0].Reason != "INVALID_FORMAT" {
				t.Fatalf("public validation loses canonical REST details: grpc=%s accepted=%t rest=%+v", status.Code(err), ok, mapped)
			}
		})
	}
	_, err := client.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{Period: 99})
	mapped, ok := finerr.ToREST(err)
	if !ok || mapped.Code != "VALIDATION_ERROR" || mapped.Spec.HTTPStatus != http.StatusBadRequest || len(mapped.Violations) != 1 || mapped.Violations[0].Field != "period" || mapped.Violations[0].Reason != "INVALID_FORMAT" {
		t.Fatalf("unknown list period loses canonical validation: accepted=%t rest=%+v", ok, mapped)
	}
	for _, tc := range []struct {
		period    commonv1.PeriodType
		key, want string
	}{
		{1, "2026-09", "2026-09"}, {2, "2026-Q3", "2026-Q3"}, {3, "2026", "2026"},
		{2, "2026-09", "2026-Q3"}, {3, "2026-09", "2026"}, {3, "2026-Q3", "2026"},
	} {
		t.Run(tc.period.String()+"/"+tc.key, func(t *testing.T) {
			got, err := client.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: tc.period, PeriodKey: proto.String(tc.key)})
			if err != nil || got.Params == nil || got.Params.PeriodKey != tc.want || got.Params.Period != tc.period {
				t.Fatalf("valid explicit/finer key regressed: response=%v error=%v", got, err)
			}
		})
	}
}
