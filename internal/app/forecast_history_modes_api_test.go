package app_test

import (
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPIForecastHistoryCannotBorrowAnotherTransferModesProvenanceOrGaps(t *testing.T) {
	env, c := startAPI(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	env.App.Now = func() time.Time { return now }
	raw, err := os.ReadFile("../../testdata/forecast_history.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	oldGapFrom, err := time.Parse(time.RFC3339, "2026-10-20T00:00:00+03:00")
	if err != nil {
		t.Fatal(err)
	}
	oldGapTo := oldGapFrom.AddDate(0, 0, 2)
	for _, period := range req.Periods {
		if period.Period == commonv1.PeriodType_PERIOD_TYPE_MONTH && period.PeriodKey == "2026-10" && period.TransferMode == commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
			for _, revision := range period.DataRevisions {
				if revision.Code == "uploads" {
					revision.Value = 1
				}
			}
			period.Bounds.Gaps = []*commonv1.TimeRange{{Start: timestamppb.New(oldGapFrom), End: timestamppb.New(oldGapTo)}}
		}
	}
	// Keeping the selected year current makes the assertion depend on the monthly inventory's mode.
	if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), req); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []commonv1.TransferMode{commonv1.TransferMode_TRANSFER_MODE_WITH, commonv1.TransferMode_TRANSFER_MODE_WITHOUT} {
		response, err := c.analytics.GetAnalytics(apitest.User(t.Context(), alice), &analyticsv1.GetAnalyticsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: proto.String("2026"), TransferMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		tiles := screenBlock(response, "tiles").Data.AsMap()["tiles"].([]any)
		if len(tiles) != 5 || tiles[1].(map[string]any)["value"] != float64(100000) {
			t.Fatal("facts changed with history provenance", tiles)
		}
		actual := tiles[4].(map[string]any)["lock"].(map[string]any)["text"].(string)
		path := "../../testdata/forecast_history_unknown.txt"
		if mode == commonv1.TransferMode_TRANSFER_MODE_WITH {
			path = "../../testdata/forecast_history_detailed.txt"
		}
		expected, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if actual != strings.TrimSpace(string(expected)) {
			t.Fatalf("mode %v:\n%s\nwant:\n%s", mode, actual, expected)
		}
	}
}
