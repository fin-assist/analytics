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
	now := func() time.Time { return req.State.CalculatedAt.AsTime() }
	repo := NewMetrics(testMetricsDB(pool), now)
	screen, err := usecase.NewScreen(repo, now)
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
			const dailyID = "daily_expense"
			const helpKey = "help_text"
			const captionKey = "caption"
			const valueKey = "value"
			const statusKey = "status"
			const expectedHelp = "По дням с данными не позже сегодня; пропуски не учитываются"
			legacyDaily := tiles[dailyID]
			if legacyDaily[helpKey] != expectedHelp || legacyDaily[captionKey] != "в августе — 2 890 ₽" || legacyDaily[valueKey] != float64(337280) || legacyDaily[statusKey] != "ready" {
				t.Fatalf("persisted daily value/comparison/methodology changed: %v", legacyDaily)
			}
			grouped, err := composer.Compose(ctx, selected, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			foundDaily := false
			for _, block := range grouped.Blocks {
				if block.Id != "tiles" {
					continue
				}
				for _, group := range block.Data.AsMap()["groups"].([]any) {
					for _, entry := range group.(map[string]any)["tiles"].([]any) {
						tile := entry.(map[string]any)
						if tile["id"] != dailyID {
							continue
						}
						foundDaily = true
						caption, _ := tile[captionKey].(string)
						if tile[helpKey] != expectedHelp || tile[captionKey] != legacyDaily[captionKey] || strings.Contains(caption, "пропуски") || tile[valueKey] != legacyDaily[valueKey] || tile[statusKey] != legacyDaily[statusKey] {
							t.Fatalf("grouping changed persisted daily values or text roles: %v", tile)
						}
					}
				}
			}
			if !foundDaily {
				t.Fatal("grouped daily expense tile missing")
			}
		})
	}
}
