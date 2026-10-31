package app_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPICombinedCalendarAllTimeAndRollingKeepTheirRangesAndFacts(t *testing.T) {
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
	ctx := apitest.User(t.Context(), alice)
	for _, tc := range []struct {
		typ commonv1.PeriodType
		key string
	}{
		{commonv1.PeriodType_PERIOD_TYPE_MONTH, "2026-09"},
		{commonv1.PeriodType_PERIOD_TYPE_ALL_TIME, "all_time"},
		{commonv1.PeriodType_PERIOD_TYPE_ROLLING_YEAR, "rolling_year"},
		{commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, "rolling_quarter"},
	} {
		for _, mode := range []commonv1.TransferMode{1, 2} {
			var legacy *analyticsv1.GetAnalyticsResponse
			for _, version := range []int32{1, 2} {
				got, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: tc.typ, PeriodKey: proto.String(tc.key), TransferMode: mode, Supports: []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: version}, {Type: "chart", MaxVersion: 1}, {Type: "ranked_list", MaxVersion: 1}}})
				if err != nil || !got.GetHasData() {
					t.Fatalf("%s mode%d v%d: %v %v", tc.key, mode, version, got, err)
				}
				if got.Params.Period != tc.typ || got.Params.PeriodKey != tc.key || got.Params.TransferMode != mode {
					t.Fatal(got.Params)
				}
				tiles := screenBlock(got, "tiles")
				if tiles == nil || tiles.V != version || tiles.Status != "ready" {
					t.Fatal(tiles)
				}
				if version == 1 {
					legacy = got
					continue
				}
				if !proto.Equal(got.Period, legacy.Period) || !proto.Equal(got.Navigation, legacy.Navigation) {
					t.Fatalf("capability changed period metadata: %v %v", legacy, got)
				}
				flat := map[string]map[string]any{}
				for _, item := range screenBlock(legacy, "tiles").Data.AsMap()["tiles"].([]any) {
					tile := item.(map[string]any)
					flat[tile["id"].(string)] = tile
				}
				groups := tiles.Data.AsMap()["groups"].([]any)
				if len(groups) != 2 {
					t.Fatal(groups)
				}
				count := 0
				for _, group := range groups {
					for _, item := range group.(map[string]any)["tiles"].([]any) {
						tile := item.(map[string]any)
						before := flat[tile["id"].(string)]
						for _, field := range []string{"status", "value", "value_kind", "action", "lock"} {
							if !reflect.DeepEqual(tile[field], before[field]) {
								t.Fatalf("%s financial %s changed: %v vs %v", tc.key, field, tile, before)
							}
						}
						count++
					}
				}
				if count != len(flat) {
					t.Fatal("tiles lost in v2")
				}
				if tc.typ != commonv1.PeriodType_PERIOD_TYPE_MONTH {
					if got.Period.IsCurrent || got.Navigation.Previous != nil || got.Navigation.Next != nil || len(got.Blocks) != 4 || flat["forecast"] != nil {
						t.Fatalf("singleton calendar leakage: %v", got)
					}
					if len(groups[1].(map[string]any)["tiles"].([]any)) != 1 {
						t.Fatal("singleton forecast invented")
					}
				}
			}
		}
		listed, err := c.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{Period: tc.typ})
		if err != nil || len(listed.Periods) != 1 || listed.Periods[0].Key != tc.key {
			t.Fatalf("%s list %v %v", tc.key, listed, err)
		}
		for _, foreign := range []string{"all_time", "rolling_year", "rolling_quarter"} {
			if foreign == tc.key {
				continue
			}
			_, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: tc.typ, PeriodKey: proto.String(foreign)})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s accepted foreign %s: %v", tc.key, foreign, err)
			}
		}
	}
}
