package postgres

import (
	"os"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationReviewCopyPreservesStoredFinancialValues(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	user := newUser(416)
	raw, err := os.ReadFile("../../../testdata/analytics.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	annual := proto.Clone(req.Periods[0]).(*analyticsv1.PeriodSnapshot)
	annual.Period = commonv1.PeriodType_PERIOD_TYPE_YEAR
	annual.PeriodKey = "2026"
	annual.Range = &commonv1.TimeRange{
		Start: at(time.Date(2026, 1, 1, 0, 0, 0, 0, msk)),
		End:   at(time.Date(2027, 1, 1, 0, 0, 0, 0, msk)),
	}
	if _, err := upsertProto(ctx, pool, user, 1, append(req.Periods, annual), nil); err != nil {
		t.Fatal(err)
	}
	state := validState()
	state.fullMonths = req.State.FullMonths
	state.dataFrom = ptr(req.State.DataFrom.AsTime())
	if _, err := upsertState(ctx, pool, user, 1, state); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	screen, err := usecase.NewScreen(repo, func() time.Time { return req.State.CalculatedAt.AsTime() })
	if err != nil {
		t.Fatal(err)
	}
	composer, err := metricsgrpc.NewComposer(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		period             domain.PeriodType
		key, forecastTitle string
	}{
		{domain.PeriodMonth, "2026-09", "Прогноз расходов"},
		{domain.PeriodYear, "2026", "Прогноз расходов на год"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			selected, err := screen.Get(ctx, user, tc.period, &tc.key, domain.TransferWith)
			if err != nil {
				t.Fatal(err)
			}
			response, err := composer.Compose(ctx, selected, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			tiles := map[string]map[string]any{}
			for _, block := range response.Blocks {
				if block.Id == "tiles" {
					for _, entry := range block.Data.AsMap()["tiles"].([]any) {
						tile := entry.(map[string]any)
						tiles[tile["id"].(string)] = tile
					}
				}
			}
			if balance := tiles["balance"]; balance["caption"] != "46% дохода" || balance["value"] != float64(7258000) {
				t.Fatalf("balance caption or value changed: %v", balance)
			}
			if forecast := tiles["forecast"]; forecast["title"] != tc.forecastTitle || forecast["value"] != float64(10120000) || forecast["approx"] != true {
				t.Fatalf("forecast title or value changed: %v", forecast)
			}
			if tiles["expense"]["value"] != float64(8432000) || tiles["income"]["value"] != float64(15690000) {
				t.Fatalf("actual financial values changed: %v", tiles)
			}
		})
	}
}
