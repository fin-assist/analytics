package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPILegacyNonCivilStateKeepsCalendarAndAllTimeReads(t *testing.T) {
	env, c := startAPI(t)
	req := rollingRequest(t)
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	all := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, all); err != nil {
		t.Fatal(err)
	}
	req.Periods = append(req.Periods, all.Periods...)
	req.Periods = append(req.Periods, analyticsFixture(t).Periods...)
	env.App.Now = func() time.Time { return req.State.CalculatedAt.AsTime() }
	if _, err = c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if _, err = conn.Exec(t.Context(), `UPDATE user_data_state SET data_to=data_to+interval '1 hour' WHERE user_id=$1`, alice); err != nil {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), alice)
	for _, tc := range []struct {
		typ commonv1.PeriodType
		key string
	}{{1, "2026-09"}, {2, "2026-Q4"}, {3, "2026"}, {4, "all_time"}} {
		got, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: tc.typ, PeriodKey: proto.String(tc.key)})
		if err != nil || !got.GetHasData() || got.GetParams().GetPeriodKey() != tc.key {
			t.Fatalf("legacy state broke %s: %v %v", tc.key, got, err)
		}
	}
	_, err = c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER})
	if status.Code(err) != codes.Internal {
		t.Fatalf("legacy rolling guard changed: %v", err)
	}
}

func TestAPIRollingListSkipsStaleWithoutForCurrentWith(t *testing.T) {
	env, c := startAPI(t)
	req := rollingRequest(t)
	env.App.Now = func() time.Time { return req.State.CalculatedAt.AsTime() }
	for _, p := range req.Periods {
		p.Bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
		p.Bounds.Gaps = nil
		p.Metrics = nil
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	current := proto.Clone(req).(*analyticsv1.UpsertUserMetricsRequest)
	current.CalcVersion++
	current.State.DataTo = timestamppb.New(current.State.DataTo.AsTime().AddDate(0, 0, 1))
	current.State.DataRevisions = revisionList(map[string]int64{"uploads": 2, "categories": 2, "time_zone": 1})
	current.Periods = nil
	for _, p := range req.Periods {
		if p.TransferMode != commonv1.TransferMode_TRANSFER_MODE_WITH {
			continue
		}
		p = proto.Clone(p).(*analyticsv1.PeriodSnapshot)
		start := "2026-08-01T00:00:00+03:00"
		if p.Period == commonv1.PeriodType_PERIOD_TYPE_ROLLING_YEAR {
			start = "2025-11-01T00:00:00+03:00"
		}
		at, err := time.Parse(time.RFC3339, start)
		if err != nil {
			t.Fatal(err)
		}
		p.Range.Start = timestamppb.New(at)
		p.Range.End = current.State.DataTo
		p.Bounds.DataFrom = p.Range.Start
		p.Bounds.DataTo = timestamppb.New(current.State.DataTo.AsTime().Add(-time.Second))
		p.DataRevisions = revisionList(map[string]int64{"uploads": 2, "categories": 1, "time_zone": 1})
		current.Periods = append(current.Periods, p)
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), current); err != nil {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), alice)
	for _, p := range current.Periods {
		got, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: p.Period, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH})
		if err != nil || got.GetPeriod().GetBounds().GetCoverage() != commonv1.Coverage_COVERAGE_COMPLETE {
			t.Fatalf("WITH facts: %v %v", got, err)
		}
		listed, err := c.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{Period: p.Period})
		if err != nil || len(listed.GetPeriods()) != 1 || listed.Periods[0].Coverage != commonv1.Coverage_COVERAGE_COMPLETE || !proto.Equal(listed.Periods[0].Range, got.Period.Range) {
			t.Fatalf("stale WITHOUT hid current WITH: %v %v", listed, err)
		}
		revisionsWant(t, listed.DataRevisions, map[string]int64{"uploads": 2, "time_zone": 1})
	}
}
