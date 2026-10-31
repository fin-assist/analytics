package postgres

import (
	"os"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationForecastHistoryCannotBorrowAnotherTransferModesProvenanceOrGaps(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	user := newUser(404)
	raw, err := os.ReadFile("../../../testdata/forecast_history.json")
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
	// Both selected year headers are current; only WITHOUT's monthly inventory is stale.
	if _, err := upsertProto(ctx, pool, user, 4, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	_, state, _ := historyFixture(t)
	if _, err := upsertState(ctx, pool, user, 4, stateRowOf(state)); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	screen, err := usecase.NewScreen(repo, func() time.Time { return *state.CalculatedAt })
	if err != nil {
		t.Fatal(err)
	}
	composer, err := metricsgrpc.NewComposer(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range domain.TransferModes {
		key := "2026"
		got, err := screen.Get(ctx, user, domain.PeriodYear, &key, mode)
		if err != nil {
			t.Fatal(err)
		}
		if mode == domain.TransferWith && (got.History == nil || len(got.History.Months) != 13) {
			t.Fatalf("WITH lost its proven inventory: %+v", got.History)
		}
		if mode == domain.TransferWithout && got.History != nil {
			t.Fatal("WITHOUT borrowed current headers from WITH", got.History)
		}
		response, err := composer.Compose(ctx, got, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		actual := ""
		for _, block := range response.Blocks {
			if block.Id != "tiles" {
				continue
			}
			for _, entry := range block.Data.AsMap()["tiles"].([]any) {
				tile := entry.(map[string]any)
				if tile["id"] == "forecast" {
					actual = tile["lock"].(map[string]any)["text"].(string)
				}
			}
		}
		path := "../../../testdata/forecast_history_unknown.txt"
		if mode == domain.TransferWith {
			path = "../../../testdata/forecast_history_detailed.txt"
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
