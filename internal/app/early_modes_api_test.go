package app_test

import (
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIGlobalEarlyCompositionIsIndependentOfDeliveryMetadataForAllPeriods(t *testing.T) {
	env, c := startAPI(t)
	zone := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, zone)
	env.App.Now = func() time.Time { return now }
	from := time.Date(2026, 10, 15, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, zone)
	revisions := revisionList(map[string]int64{"categories": 0, "uploads": 2, "time_zone": 0})
	req := &analyticsv1.UpsertUserMetricsRequest{UserId: alice, CalcVersion: 4, State: &analyticsv1.UserDataState{HasData: true, DataFrom: timestamppb.New(from), DataTo: timestamppb.New(to), LastOperationAt: timestamppb.New(from), StaleAfter: timestamppb.New(to.AddDate(0, 0, 14)), TimeZone: zone.String(), CalculatedAt: timestamppb.New(now), DataRevisions: revisions}}
	// Use IANA just as the real profile; the fixed zone above only makes timestamp fixture construction explicit.
	req.State.TimeZone = "Europe/Moscow"
	periods := []struct {
		typ           commonv1.PeriodType
		key           string
		month, months int
	}{{commonv1.PeriodType_PERIOD_TYPE_MONTH, "2026-10", 10, 1}, {commonv1.PeriodType_PERIOD_TYPE_QUARTER, "2026-Q4", 10, 3}, {commonv1.PeriodType_PERIOD_TYPE_YEAR, "2026", 1, 12}}
	for _, period := range periods {
		start := time.Date(2026, time.Month(period.month), 1, 0, 0, 0, 0, zone)
		end := start.AddDate(0, period.months, 0)
		for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
			req.Periods = append(req.Periods, &analyticsv1.PeriodSnapshot{Period: period.typ, PeriodKey: period.key, TransferMode: mode, Range: &commonv1.TimeRange{Start: timestamppb.New(start), End: timestamppb.New(end)}, Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: timestamppb.New(from), DataTo: timestamppb.New(to.Add(-time.Second))}, CalculatedAt: timestamppb.New(now), DataRevisions: revisions,
				Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "monthly_chart", DefinitionVersion: 1, Metric: &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_LOCKED, Lock: &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_NEED_MONTHS_WITH_DATA, Required: proto.Int32(2), Available: proto.Int32(1)}}}}})
		}
	}
	for _, known := range []bool{true, false} {
		if !known {
			req.CalcVersion = 5
			req.State.DataRevisions = nil
			for _, period := range req.Periods {
				period.DataRevisions = nil
			}
		}
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
			t.Fatal(err)
		}
		for _, period := range periods {
			got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: period.typ, PeriodKey: proto.String(period.key), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT})
			if err != nil {
				t.Fatal(err)
			}
			if screenBlock(got, "monthly_chart") != nil {
				t.Fatalf("period %s known=%v fabricated chart", period.key, known)
			}
		}
	}
}
