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
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	to := from.AddDate(0, 1, 0)
	calculated := to.Add(-time.Hour)
	now := calculated
	env.App.Now = func() time.Time { return now }
	revisions := revisionList(map[string]int64{"categories": 0, "uploads": 2, "time_zone": 0})
	req := &analyticsv1.UpsertUserMetricsRequest{UserId: alice, State: &analyticsv1.UserDataState{HasData: true, DataFrom: timestamppb.New(from), DataTo: timestamppb.New(to), LastOperationAt: timestamppb.New(from), StaleAfter: timestamppb.New(to.AddDate(0, 0, 14)), TimeZone: "Europe/Moscow", DataRevisions: revisions}}
	periods := []struct {
		typ           commonv1.PeriodType
		key           string
		month, months int
	}{{commonv1.PeriodType_PERIOD_TYPE_MONTH, "2026-10", 10, 1}, {commonv1.PeriodType_PERIOD_TYPE_QUARTER, "2026-Q4", 10, 3}, {commonv1.PeriodType_PERIOD_TYPE_YEAR, "2026", 1, 12}}
	for _, period := range periods {
		start := time.Date(2026, time.Month(period.month), 1, 0, 0, 0, 0, zone)
		end := start.AddDate(0, period.months, 0)
		for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
			req.Periods = append(req.Periods, &analyticsv1.PeriodSnapshot{Period: period.typ, PeriodKey: period.key, TransferMode: mode, Range: &commonv1.TimeRange{Start: timestamppb.New(start), End: timestamppb.New(end)}, Bounds: &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: timestamppb.New(from), DataTo: timestamppb.New(to.Add(-time.Second))}, CalculatedAt: timestamppb.New(calculated), DataRevisions: revisions,
				Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "monthly_chart", DefinitionVersion: 1, Metric: &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_LOCKED, Lock: &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_NEED_MONTHS_WITH_DATA, Required: proto.Int32(2), Available: proto.Int32(1)}}}}})
		}
	}
	for _, known := range []bool{true, false} {
		req.UserId = alice
		version := int32(4)
		if !known {
			req.UserId = bob
			version = 6
			req.State.DataRevisions = nil
			for _, period := range req.Periods {
				period.DataRevisions = nil
			}
		}
		now = calculated
		req.CalcVersion = version
		req.State.CalculatedAt = timestamppb.New(calculated)
		req.State.FullMonths = 0
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
			t.Fatal(err)
		}
		check := func(visible bool) {
			t.Helper()
			for _, period := range periods {
				got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), req.UserId), &analyticsv1.GetAnalyticsRequest{Period: period.typ, PeriodKey: proto.String(period.key), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT})
				if err != nil {
					t.Fatal(err)
				}
				block := screenBlock(got, "monthly_chart")
				if visible {
					if block == nil || block.Status != "locked" || block.Lock.GetReason() != "need_months_with_data" || block.Lock.GetAvailable() != 1 {
						t.Fatalf("period %s known=%v chart=%v", period.key, known, block)
					}
				} else if block != nil {
					t.Fatalf("period %s known=%v fabricated chart", period.key, known)
				}
			}
		}
		check(false)
		now = to.Add(10 * time.Hour)
		check(true)
		req.State.CalculatedAt = timestamppb.New(now)
		req.State.FullMonths = 1
		daily := &analyticsv1.UpsertUserMetricsRequest{UserId: req.UserId, CalcVersion: version + 1, State: req.State}
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), daily); err != nil {
			t.Fatal(err)
		}
		check(true)
	}
}
