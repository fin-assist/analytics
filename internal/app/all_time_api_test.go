package app_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIAllTimeCapabilitiesAndSubjectRemainBoundToAuthenticatedUser(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	raw, err := os.ReadFile("../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	userCtx := apitest.User(t.Context(), alice)
	for _, mode := range []commonv1.TransferMode{1, 2} {
		for _, v := range []int32{1, 2} {
			r, e := c.analytics.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Period: 4, TransferMode: mode, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 1}, {Type: "tile_group", MaxVersion: v}, {Type: "chart", MaxVersion: 1}, {Type: "ranked_list", MaxVersion: 1}}})
			if e != nil || !r.HasData || r.Params.PeriodKey != "all_time" || r.Params.TransferMode != mode || r.Period.IsCurrent || len(r.Blocks) != 4 || r.Period.Bounds.Coverage != 2 {
				t.Fatalf("%v %v", r, e)
			}
			b := screenBlock(r, "tiles")
			if b.V != v {
				t.Fatal(b)
			}
			var expense map[string]any
			if v == 2 {
				gs := b.Data.AsMap()["groups"].([]any)
				expense = gs[0].(map[string]any)["tiles"].([]any)[0].(map[string]any)
				if len(gs[1].(map[string]any)["tiles"].([]any)) != 1 {
					t.Fatal("forecast invented")
				}
			} else {
				expense = b.Data.AsMap()["tiles"].([]any)[1].(map[string]any)
			}
			want := float64(35000)
			if mode == 2 {
				want = 30000
			}
			if expense["value"] != want {
				t.Fatal(expense)
			}
			ps := screenBlock(r, "monthly_chart").Data.AsMap()["points"].([]any)
			if len(ps) != 11 || ps[0].(map[string]any)["action"].(map[string]any)["date"] != "2025-12" || ps[1].(map[string]any)["value"] != float64(0) || ps[2].(map[string]any)["action"] != nil {
				t.Fatal(ps)
			}
		}
	}
	legacy, e := c.analytics.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Period: 4, PeriodKey: proto.String("all_time")})
	if e != nil || screenBlock(legacy, "tiles").V != 1 {
		t.Fatalf("%v %v", legacy, e)
	}
	list, e := c.analytics.ListPeriods(userCtx, &analyticsv1.ListPeriodsRequest{Period: 4})
	if e != nil || len(list.Periods) != 1 || list.Periods[0].Key != "all_time" {
		t.Fatalf("%v %v", list, e)
	}
	for _, bad := range []*analyticsv1.GetAnalyticsRequest{{Period: 5}, {Period: 4, PeriodKey: proto.String("2026-10")}, {Period: 1, PeriodKey: proto.String("all_time")}, {Period: 4, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 0}}}, {Period: 4, Supports: []*commonv1.BlockSupport{{Type: "future", MaxVersion: -1}}}} {
		_, e := c.analytics.GetAnalytics(userCtx, bad)
		r, ok := finerr.ToREST(e)
		if !ok || status.Code(e) != codes.InvalidArgument || len(r.Violations) != 1 || r.Violations[0].Reason != "INVALID_FORMAT" {
			t.Fatal(e)
		}
	}
	other, e := c.analytics.GetAnalytics(apitest.User(t.Context(), bob), &analyticsv1.GetAnalyticsRequest{Period: 4})
	if e != nil || other.HasData || other.Params != nil || len(other.Blocks) != 0 {
		t.Fatalf("isolation %v %v", other, e)
	}
	_, e = c.analytics.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Period: 4, UserId: proto.String(bob)})
	wantBare(t, e, codes.InvalidArgument)
	_, e = c.analytics.GetAnalytics(apitest.Caller(t.Context(), "bank-statement-data"), &analyticsv1.GetAnalyticsRequest{Period: 4, UserId: proto.String(alice)})
	wantBare(t, e, codes.PermissionDenied)
	assistant, e := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "assistant"), &analyticsv1.GetAnalyticsRequest{Period: 4, UserId: proto.String(alice)})
	if e != nil || !assistant.HasData {
		t.Fatalf("%v %v", assistant, e)
	}
	if _, e = c.deletion.PurgeUserData(apitest.Caller(t.Context(), "user"), &userdeletionv1.PurgeUserDataRequest{UserId: alice}); e != nil {
		t.Fatal(e)
	}
	req.CalcVersion++
	late, e := c.ingest.UpsertUserMetrics(producer(t.Context()), req)
	if e != nil || late.AppliedPeriods != 0 || rowCount(t, env, alice) != 0 {
		t.Fatalf("late all_time revival %v %v", late, e)
	}
	_, e = c.analytics.GetAnalytics(userCtx, &analyticsv1.GetAnalyticsRequest{Period: 4})
	if apitest.Reason(e) != "TOKEN_REVOKED" {
		t.Fatal(e)
	}
}

func TestAPICalendarSummaryGroupsKeepForecastFactsAndEmptyState(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), analyticsFixture(t)); err != nil {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), alice)
	read := func(key string, v int32) *analyticsv1.GetAnalyticsResponse {
		t.Helper()
		r, e := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: &key, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: v}}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	old := screenBlock(read("2026-09", 1), "tiles").Data.AsMap()["tiles"].([]any)
	groups := screenBlock(read("2026-09", 2), "tiles").Data.AsMap()["groups"].([]any)
	primary := groups[0].(map[string]any)["tiles"].([]any)
	extra := groups[1].(map[string]any)["tiles"].([]any)
	if len(primary) != 3 || len(extra) != 2 || extra[0].(map[string]any)["id"] != "daily_expense" || extra[1].(map[string]any)["id"] != "forecast" || extra[1].(map[string]any)["value"] != float64(10120000) {
		t.Fatal(groups)
	}
	for i, id := range []string{"expense", "income", "balance"} {
		m := primary[i].(map[string]any)
		legacy := old[map[string]int{"balance": 0, "expense": 1, "income": 3}[id]].(map[string]any)
		if m["id"] != id || m["value"] != legacy["value"] || !reflect.DeepEqual(m["action"], legacy["action"]) {
			t.Fatal(m)
		}
	}
	empty := screenBlock(read("2025-01", 2), "tiles")
	if empty.Status != "none" || empty.V != 2 || len(empty.Data.AsMap()["groups"].([]any)) != 0 {
		t.Fatal(empty)
	}
}
