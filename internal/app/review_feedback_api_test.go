package app_test

import (
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIMarkerUsesVerifiedCallerAndPrimaryEffectiveFlag(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	ctx := t.Context()
	marker := &analyticsv1.MarkRecalculatingRequest{UserId: alice, CalcVersion: 11}
	for _, caller := range []string{"bank-bff", "assistant", "user"} {
		_, err := c.ingest.MarkRecalculating(apitest.Caller(ctx, caller), marker)
		wantBare(t, err, codes.PermissionDenied)
	}
	_, err := c.ingest.MarkRecalculating(apitest.User(ctx, alice), marker)
	wantBare(t, err, codes.PermissionDenied)
	for _, version := range []int32{0, -1} {
		_, err = c.ingest.MarkRecalculating(producer(ctx), &analyticsv1.MarkRecalculatingRequest{UserId: alice, CalcVersion: version})
		wantBare(t, err, codes.InvalidArgument)
	}
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), req); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ingest.MarkRecalculating(producer(ctx), marker); err != nil {
		t.Fatal(err)
	}
	get := func() *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		result, err := c.analytics.GetAnalytics(apitest.User(ctx, alice), &analyticsv1.GetAnalyticsRequest{Period: 4, PeriodKey: proto.String("all_time")})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	got := get()
	if len(got.DataRevisions) != 0 || got.Calculation.GetCalcVersion() != 10 || !got.Calculation.GetRecalculating() || !got.State.GetRecalculating() || got.TimeZone != "Europe/Moscow" {
		t.Fatal(got)
	}
	list, err := c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &analyticsv1.ListPeriodsRequest{UserId: proto.String(alice), Period: commonv1.PeriodType_PERIOD_TYPE_ALL_TIME})
	if err != nil || len(list.DataRevisions) != 0 {
		t.Fatalf("marker certified previous sources: %v %v", list, err)
	}
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), req); err != nil {
		t.Fatal(err)
	}
	if !get().Calculation.GetRecalculating() {
		t.Fatal("stale false cleared acknowledged marker")
	}
	// The unchanged producer final path clears readiness once its applied version covers the target.
	req.CalcVersion = 12
	req.Periods = nil
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), req); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ingest.MarkRecalculating(producer(ctx), marker); err != nil {
		t.Fatal(err)
	}
	got = get()
	if len(got.DataRevisions) != 3 || got.Calculation.GetCalcVersion() != 12 || got.Calculation.GetRecalculating() || got.State.GetRecalculating() {
		t.Fatal("late marker revived old work", got)
	}
	// Acknowledged true survives the no-financial-state case as required metadata.
	if _, err = c.ingest.MarkRecalculating(producer(ctx), &analyticsv1.MarkRecalculatingRequest{UserId: bob, CalcVersion: 1}); err != nil {
		t.Fatal(err)
	}
	empty, err := c.analytics.GetAnalytics(apitest.User(ctx, bob), &analyticsv1.GetAnalyticsRequest{})
	if err != nil || empty.HasData || empty.Calculation == nil || empty.Calculation.GetCalcVersion() != 0 || !empty.Calculation.GetRecalculating() {
		t.Fatalf("empty marker %v %v", empty, err)
	}
	if _, err = c.deletion.PurgeUserData(apitest.Caller(ctx, "user"), &userdeletionv1.PurgeUserDataRequest{UserId: alice}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ingest.MarkRecalculating(producer(ctx), marker); err != nil {
		t.Fatal("deleted marker should ACK without revival", err)
	}
	if rows := rowCount(t, env, alice); rows != 0 {
		t.Fatal("late marker revived financial state", rows)
	}
}

func TestAPISourceZoneRemainsOwnedBySnapshotAndMixedListRecovers(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	request := upsert(alice, 1, append(both("2026-09", 1, commonv1.Coverage_COVERAGE_COMPLETE), both("2026-10", 1, commonv1.Coverage_COVERAGE_COMPLETE)...))
	write := func() {
		t.Helper()
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), request); err != nil {
			t.Fatal(err)
		}
	}
	write()
	user := apitest.User(t.Context(), alice)
	list, err := c.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{})
	if err != nil || len(list.Periods) != 2 || list.TimeZone != "Europe/Moscow" {
		t.Fatalf("original list %v %v", list, err)
	}
	request.CalcVersion = 2
	request.State.TimeZone = "UTC"
	request.State.Recalculating = true
	request.State.DataFrom = at(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	request.State.DataTo = at(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	request.State.StaleAfter = at(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
	// A publication chunk carries its actual IANA zone even while other stored periods retain theirs.
	newMonths := func(key string) []*analyticsv1.PeriodSnapshot {
		result := both(key, 2, commonv1.Coverage_COVERAGE_COMPLETE)
		start, _ := time.Parse("2006-01", key)
		end := start.AddDate(0, 1, 0)
		for _, snap := range result {
			snap.Range = &commonv1.TimeRange{Start: at(start), End: at(end)}
			snap.Bounds.DataFrom = at(start)
			snap.Bounds.DataTo = at(end.Add(-time.Second))
		}
		return result
	}
	request.Periods = newMonths("2026-10")
	write()
	_, err = c.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{})
	mapped, ok := finerr.ToREST(err)
	if !ok || mapped.Code != "INTERNAL_ERROR" || mapped.RequestRetryable == nil || !*mapped.RequestRetryable {
		t.Fatalf("mixed list must fail closed %v", err)
	}
	retained, err := c.analytics.GetAnalytics(user, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09")})
	if err != nil || retained.TimeZone != "Europe/Moscow" || !retained.Calculation.GetRecalculating() {
		t.Fatalf("retained source zone %v %v", retained, err)
	}
	request.Periods = newMonths("2026-09")
	write()
	request.CalcVersion = 3
	request.Periods = nil
	request.State.Recalculating = false
	write()
	list, err = c.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{})
	if err != nil || len(list.Periods) != 2 || list.TimeZone != "UTC" {
		t.Fatalf("recovered list %v %v", list, err)
	}
	ready, err := c.analytics.GetAnalytics(user, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09")})
	if err != nil || ready.TimeZone != "UTC" || ready.Calculation.GetRecalculating() || ready.Calculation.GetCalcVersion() != 3 {
		t.Fatalf("ready source zone %v %v", ready, err)
	}
}

func TestAPIBrokenAllTimeChartDoesNotHideHealthyFacts(t *testing.T) {
	env, c := startAPI(t)
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	env.App.Now = func() time.Time { return r.State.CalculatedAt.AsTime() }
	if _, err = c.ingest.UpsertUserMetrics(producer(t.Context()), r); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []string{`{"monthly_chart":"broken"}`, `{"monthly_chart":{"points":[{"month":"2026-10","expense_minor":"invalid"}]}}`} {
		if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE period_metric SET details=$1::jsonb WHERE user_id=$2 AND period_type=4 AND metric_id='monthly_chart'`, broken, alice); err != nil {
			t.Fatal(err)
		}
		got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ALL_TIME})
		if err != nil || screenBlock(got, "tiles").Status != "ready" || screenBlock(got, "monthly_chart").Status != "unavailable" {
			t.Fatalf("corrupt chart hid facts: %v %v", got, err)
		}
	}
}

func TestAPIMalformedFirstChartMonthDoesNotHideSelectedFacts(t *testing.T) {
	env, c := startAPI(t)
	r := analyticsFixture(t)
	env.App.Now = func() time.Time { return r.State.CalculatedAt.AsTime() }
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), r); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "2026-99", "not-a-month"} {
		if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE period_metric SET details=jsonb_set(details,'{monthly_chart,points,0,month}',to_jsonb($1::text)) WHERE user_id=$2 AND period_type=4 AND metric_id='monthly_chart'`, bad, alice); err != nil {
			t.Fatal(err)
		}
		for _, charts := range []bool{true, false} {
			req := &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09")}
			if !charts {
				req.Supports = []*commonv1.BlockSupport{{Type: "ranked_list", MaxVersion: 1}}
			}
			got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), req)
			if err != nil || screenBlock(got, "expense_categories").Status != "ready" || screenBlock(got, "expense_categories").Data.AsMap()["total"].(map[string]any)["value"] != float64(8432000) {
				t.Fatalf("bad month hid facts: %v %v", got, err)
			}
			chart := screenBlock(got, "monthly_chart")
			if charts && (chart == nil || chart.Status != "unavailable") || !charts && chart != nil {
				t.Fatal(chart)
			}
		}
	}
}

func TestAPILegacySourceZoneRequiresFullRepublishingBeforeReadsResume(t *testing.T) {
	env, c := startAPI(t)
	r := analyticsFixture(t)
	env.App.Now = func() time.Time { return r.State.CalculatedAt.AsTime() }
	write := func() {
		t.Helper()
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), r); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE period_snapshot SET time_zone=NULL WHERE user_id=$1`, alice); err != nil {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), alice)
	_, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09")})
	mapped, ok := finerr.ToREST(err)
	if !ok || mapped.Code != "INTERNAL_ERROR" || mapped.RequestRetryable == nil || !*mapped.RequestRetryable {
		t.Fatal(err)
	}
	_, err = c.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{})
	if apitest.Reason(err) != "INTERNAL_ERROR" {
		t.Fatal(err)
	}
	r.CalcVersion++
	write()
	got, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if err != nil || !got.HasData || got.TimeZone != "Europe/Moscow" {
		t.Fatal(got, err)
	}
	list, err := c.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{})
	if err != nil || len(list.Periods) != 1 || list.TimeZone != "Europe/Moscow" {
		t.Fatal(list, err)
	}
}

func TestAPIAllTimePeakKeepsEarliestMonthWhenValuesTie(t *testing.T) {
	env, c := startAPI(t)
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	env.App.Now = func() time.Time { return r.State.CalculatedAt.AsTime() }
	for _, amount := range []int64{0, 50000} {
		r.CalcVersion++
		for _, snap := range r.Periods {
			for _, m := range snap.Metrics {
				if m.MetricId == "monthly_chart" {
					for _, point := range m.Metric.GetMonthlyChart().Points {
						if point.ExpenseMinor != nil {
							point.ExpenseMinor = proto.Int64(amount)
						}
					}
				}
			}
		}
		if _, err = c.ingest.UpsertUserMetrics(producer(t.Context()), r); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			got, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ALL_TIME})
			if err != nil {
				t.Fatal(err)
			}
			notes := screenBlock(got, "monthly_chart").Data.AsMap()["notes"].([]any)
			if len(notes) != 1 || !strings.Contains(notes[0].(map[string]any)["text"].(string), "в декабре 2025:") {
				t.Fatal(notes)
			}
		}
	}
}
