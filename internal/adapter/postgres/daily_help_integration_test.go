package postgres

import (
	"fmt"
	"os"
	"reflect"
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

const persistedCopyReadyStatus = "ready"

func TestIntegrationDailyHelpPreservesEveryPersistedPeriodModeAndVersion(t *testing.T) {
	const calendarFixture = "analytics.json"
	const rollingFixture = "rolling.json"
	const dayComparisonID = "tiles.daily_expense.comparison"
	const expenseID = "expense"
	const dailyID = "daily_expense"
	const valueKey = "value"
	const statusKey = "status"
	const amountKindKey = "value_kind"
	const actionKey = "action"
	const expenseKind = "expense"
	const initialVersion = 1
	const calendarDailyValue = float64(337280)
	const rollingDailyValue = float64(3)
	calendarDaily := [2]float64{calendarDailyValue, calendarDailyValue}
	calendarExpense := [2]float64{8432000, 8000000}
	rollingDaily := [2]float64{rollingDailyValue, rollingDailyValue}
	rollingExpense := [2]float64{250, 200}
	const amountKind = "amount"
	const expectedHelp = "По дням с данными не позже сегодня; пропуски не учитываются"
	const calendarBase = "в среднем за 1–25 сентября"
	const rollingBase = "по покрытым дням"
	const periodEnd = "2026-10-31T00:00:00+03:00"
	const calendarEnd = "2026-10-01T00:00:00+03:00"
	for _, tc := range []struct {
		fixture, key, caption, from, to string
		typ                             domain.PeriodType
		daily, expense                  [2]float64
	}{
		{calendarFixture, "2026-09", "в августе — 2 890 ₽", "2026-09-01T00:00:00+03:00", calendarEnd, domain.PeriodMonth, calendarDaily, calendarExpense},
		{calendarFixture, "2026-Q3", calendarBase, "2026-07-01T00:00:00+03:00", calendarEnd, domain.PeriodQuarter, calendarDaily, calendarExpense},
		{calendarFixture, "2026", calendarBase, "2026-01-01T00:00:00+03:00", "2027-01-01T00:00:00+03:00", domain.PeriodYear, calendarDaily, calendarExpense},
		{"all_time.json", "all_time", "в среднем за 30 декабря 2025 — 30 октября 2026", "2025-12-30T00:00:00+03:00", periodEnd, domain.PeriodAllTime, [2]float64{555, 476}, [2]float64{35000, 30000}},
		{rollingFixture, "rolling_year", rollingBase, "2025-10-31T00:00:00+03:00", periodEnd, domain.PeriodRollingYear, rollingDaily, rollingExpense},
		{rollingFixture, "rolling_quarter", rollingBase, "2026-07-31T00:00:00+03:00", periodEnd, domain.PeriodRollingQuarter, rollingDaily, rollingExpense},
	} {
		t.Run(tc.key, func(t *testing.T) {
			pool := openDB(t)
			ctx := t.Context()
			user := newUser(429)
			raw, err := os.ReadFile("../../../testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			req := &analyticsv1.UpsertUserMetricsRequest{}
			if err = protojson.Unmarshal(raw, req); err != nil {
				t.Fatal(err)
			}
			var snapshots []*analyticsv1.PeriodSnapshot
			for _, source := range req.Periods {
				if tc.fixture != calendarFixture && domain.PeriodType(source.Period) != tc.typ {
					continue
				}
				snapshot := proto.Clone(source).(*analyticsv1.PeriodSnapshot)
				if tc.typ == domain.PeriodQuarter || tc.typ == domain.PeriodYear {
					snapshot.Period, snapshot.PeriodKey = commonv1.PeriodType(tc.typ), tc.key
					start, parseErr := time.Parse(time.RFC3339, tc.from)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					end, parseErr := time.Parse(time.RFC3339, tc.to)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					snapshot.Range = &commonv1.TimeRange{Start: at(start), End: at(end)}
					kept := snapshot.Metrics[:0]
					for _, metric := range snapshot.Metrics {
						// Quarter/year exercise the covered-period base when no comparison was delivered.
						if metric.MetricId == dayComparisonID {
							continue
						}
						if filter := metric.Metric.Filters; filter != nil {
							filter.From, filter.To = at(start), at(end)
						}
						kept = append(kept, metric)
					}
					snapshot.Metrics = kept
				}
				snapshots = append(snapshots, snapshot)
			}
			if len(snapshots) != 2 {
				t.Fatalf("want both persisted modes, got %d snapshots", len(snapshots))
			}
			if _, err = upsertProto(ctx, pool, user, initialVersion, snapshots, nil); err != nil {
				t.Fatal(err)
			}
			state := stateRow{hasData: req.State.HasData, dataFrom: ptr(req.State.DataFrom.AsTime()), dataTo: ptr(req.State.DataTo.AsTime()),
				lastOperation: ptr(req.State.LastOperationAt.AsTime()), stale: ptr(req.State.StaleAfter.AsTime()), fullMonths: req.State.FullMonths,
				recalculating: req.State.Recalculating, timeZone: req.State.TimeZone, calculatedAt: ptr(req.State.CalculatedAt.AsTime())}
			if _, err = upsertState(ctx, pool, user, initialVersion, state); err != nil {
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
			for modeIndex, mode := range []domain.TransferMode{domain.TransferWith, domain.TransferWithout} {
				for _, version := range []int32{1, 2} {
					t.Run(fmt.Sprintf("mode%d/v%d", mode, version), func(t *testing.T) {
						selected, err := screen.Get(t.Context(), user, tc.typ, &tc.key, mode)
						if err != nil {
							t.Fatal(err)
						}
						response, err := composer.Compose(t.Context(), selected, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: version}}, nil)
						if err != nil {
							t.Fatal(err)
						}
						tiles := persistedCopyTiles(t, response, version)
						daily := tiles[dailyID]
						if daily["help_text"] != expectedHelp || daily["caption"] != tc.caption || daily[valueKey] != tc.daily[modeIndex] || daily[statusKey] != persistedCopyReadyStatus || daily[amountKindKey] != amountKind || daily[actionKey] != nil {
							t.Fatalf("persisted daily facts/text roles changed: %v", daily)
						}
						expense := tiles[expenseID]
						if expense[valueKey] != tc.expense[modeIndex] || expense[statusKey] != persistedCopyReadyStatus || expense[amountKindKey] != amountKind {
							t.Fatalf("persisted financial fields changed: %v", expense)
						}
						modeName := "with"
						if mode == domain.TransferWithout {
							modeName = "without"
						}
						wantAction := map[string]any{"type": "open_operations", "filter": map[string]any{"from": tc.from, "to": tc.to, "kind": expenseKind, "transfer_mode": modeName}}
						if !reflect.DeepEqual(expense[actionKey], wantAction) {
							t.Fatalf("persisted action changed: got %v want %v", expense[actionKey], wantAction)
						}
					})
				}
			}
		})
	}
}

func persistedCopyTiles(t *testing.T, response *analyticsv1.GetAnalyticsResponse, version int32) map[string]map[string]any {
	t.Helper()
	const tilesKey = "tiles"
	for _, block := range response.Blocks {
		if block.Id != tilesKey {
			continue
		}
		if block.V != version || block.Status != persistedCopyReadyStatus {
			t.Fatalf("tile group changed: %v", block)
		}
		payload := block.Data.AsMap()
		var entries []any
		if version == 1 {
			entries = payload[tilesKey].([]any)
		} else {
			for _, group := range payload["groups"].([]any) {
				entries = append(entries, group.(map[string]any)[tilesKey].([]any)...)
			}
		}
		result := make(map[string]map[string]any, len(entries))
		for _, entry := range entries {
			tile := entry.(map[string]any)
			result[tile["id"].(string)] = tile
		}
		return result
	}
	t.Fatal("tile group missing")
	return nil
}
