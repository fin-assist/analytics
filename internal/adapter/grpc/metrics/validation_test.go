package metrics

import (
	"context"
	"net/http"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

type validationStore struct {
	reads int
	key   string
}

func (s *validationStore) ReadAnalytics(_ context.Context, _ string, _ domain.PeriodType, key *string, _ domain.TransferMode) (domain.AnalyticsData, error) {
	s.reads++
	if key != nil {
		s.key = *key
	}
	return domain.AnalyticsData{}, nil
}

func TestUnitAnalyticsPeriodValidationRemainsRESTCompatible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		period commonv1.PeriodType
		key    string
	}{
		{"year to month", commonv1.PeriodType_PERIOD_TYPE_MONTH, "2026"},
		{"year to quarter", commonv1.PeriodType_PERIOD_TYPE_QUARTER, "2026"},
		{"quarter to month", commonv1.PeriodType_PERIOD_TYPE_MONTH, "2026-Q3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &validationStore{}
			screen, err := usecase.NewScreen(store, func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) })
			if err != nil {
				t.Fatal(err)
			}
			handler := &analytics{screen: screen, composer: composerForTest(t)}
			_, err = handler.GetAnalytics(reqctx.WithUserID(t.Context(), user), &analyticsv1.GetAnalyticsRequest{Period: tc.period, PeriodKey: proto.String(tc.key)})
			translated, ok := finerr.ToREST(err)
			if status.Code(err) != codes.InvalidArgument || !ok || translated.Code != "VALIDATION_ERROR" || translated.Spec.HTTPStatus != http.StatusBadRequest || len(translated.Violations) != 1 || translated.Violations[0].Field != "date" || translated.Violations[0].Reason != "INVALID_FORMAT" {
				t.Fatalf("period validation cannot be exposed as REST400: grpc=%s accepted=%t REST=%+v", status.Code(err), ok, translated)
			}
			if store.reads != 0 {
				t.Fatal("malformed period reached storage")
			}
		})
	}
}

func TestUnitAnalyticsValidationFieldsRemainRESTCompatible(t *testing.T) {
	ctx := reqctx.WithUserID(t.Context(), user)
	for _, tc := range []struct {
		name, field string
		call        func(*analytics) error
	}{
		{"screen period", "period", func(h *analytics) error {
			_, err := h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 99})
			return err
		}},
		{"screen mode", "transfer_mode", func(h *analytics) error {
			_, err := h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{TransferMode: 99})
			return err
		}},
		{"list period", "period", func(h *analytics) error {
			_, err := h.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{Period: 99})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			translated, ok := finerr.ToREST(tc.call(&analytics{}))
			if !ok || translated.Code != "VALIDATION_ERROR" || translated.Spec.HTTPStatus != http.StatusBadRequest || len(translated.Violations) != 1 || translated.Violations[0].Field != tc.field || translated.Violations[0].Reason != "INVALID_FORMAT" {
				t.Fatalf("validation field cannot be exposed as REST400: accepted=%t REST=%+v", ok, translated)
			}
		})
	}
}

func TestUnitAnalyticsAcceptedPeriodKeysReachStorageUnchangedOrCoarsened(t *testing.T) {
	for _, tc := range []struct {
		period    commonv1.PeriodType
		key, want string
	}{
		{1, "2026-09", "2026-09"}, {2, "2026-Q3", "2026-Q3"}, {3, "2026", "2026"},
		{2, "2026-09", "2026-Q3"}, {3, "2026-09", "2026"}, {3, "2026-Q3", "2026"},
	} {
		t.Run(tc.period.String()+"/"+tc.key, func(t *testing.T) {
			store := &validationStore{}
			screen, err := usecase.NewScreen(store, func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) })
			if err != nil {
				t.Fatal(err)
			}
			h := &analytics{screen: screen, composer: composerForTest(t)}
			response, err := h.GetAnalytics(reqctx.WithUserID(t.Context(), user), &analyticsv1.GetAnalyticsRequest{Period: tc.period, PeriodKey: proto.String(tc.key)})
			if err != nil || response == nil || store.reads != 1 || store.key != tc.want {
				t.Fatalf("accepted key changed: response=%v err=%v reads=%d key=%q", response, err, store.reads, store.key)
			}
		})
	}
}
