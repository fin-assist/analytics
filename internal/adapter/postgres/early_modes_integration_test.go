package postgres

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationGlobalEarlyCompositionIsIndependentOfDeliveryMetadataForAllPeriods(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	rows, state, _ := historyFixture(t)
	row := rows[len(rows)-1]
	from, end := *row.Range.Start, *row.Range.End
	last := end.Add(-time.Second)
	calculated := end.Add(-time.Hour)
	state.DataFrom = &from
	state.DataTo = &end
	state.LastOperationAt = &from
	stale := end.AddDate(0, 0, 14)
	state.StaleAfter = &stale
	clock := calculated
	periods := []struct {
		typ           domain.PeriodType
		key           string
		month, months int
	}{{domain.PeriodMonth, "2026-10", 10, 1}, {domain.PeriodQuarter, "2026-Q4", 10, 3}, {domain.PeriodYear, "2026", 1, 12}}
	snapshots := []domain.Snapshot{}
	for _, period := range periods {
		start := time.Date(2026, time.Month(period.month), 1, 0, 0, 0, 0, from.Location())
		stop := start.AddDate(0, period.months, 0)
		for _, mode := range domain.TransferModes {
			snapshots = append(snapshots, domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: period.typ, Key: period.key}, Mode: mode}, Range: &domain.TimeRange{Start: &start, End: &stop}, Coverage: domain.CoveragePartial, DataFrom: &from, DataTo: &last, CalculatedAt: &calculated, Revisions: state.Revisions,
				Metrics: []domain.Metric{{ID: "monthly_chart", DefinitionVersion: 1, Status: 3, Lock: &domain.Lock{Reason: 2, Required: ptr(int32(2)), Available: ptr(int32(1))}}}})
		}
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	screen, err := usecase.NewScreen(repo, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	composer, err := metricsgrpc.NewComposer(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, known := range []bool{true, false} {
		user, version := newUser(405), int32(4)
		if !known {
			user = newUser(406)
			version = 6
			state.Revisions = nil
			for i := range snapshots {
				snapshots[i].Revisions = nil
			}
		}
		clock = calculated
		state.CalculatedAt = &calculated
		state.FullMonths = 0
		if _, err := upsertPeriods(ctx, pool, user, version, snapshots, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := upsertState(ctx, pool, user, version, stateRowOf(state)); err != nil {
			t.Fatal(err)
		}
		check := func(rollover, visible bool) {
			t.Helper()
			for _, period := range periods {
				got, err := screen.Get(ctx, user, period.typ, &period.key, domain.TransferWithout)
				if err != nil {
					t.Fatal(err)
				}
				if got.Rollover != rollover {
					t.Fatalf("period %s rollover=%v want%v", period.key, got.Rollover, rollover)
				}
				if (got.History != nil) != (known && !rollover) {
					t.Fatalf("period %s known=%v rollover=%v history=%+v", period.key, known, rollover, got.History)
				}
				response, err := composer.Compose(ctx, got, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, block := range response.Blocks {
					if block.Id == "monthly_chart" {
						found = true
						if block.Status != "locked" || block.Lock.GetReason() != "need_months_with_data" || block.Lock.GetAvailable() != 1 {
							t.Fatal(block)
						}
					}
				}
				if found != visible {
					t.Fatalf("period %s known=%v rollover=%v chart=%v want%v", period.key, known, rollover, found, visible)
				}
			}
		}
		check(false, false)
		clock = end.Add(10 * time.Hour)
		check(true, true)
		daily := clock
		state.CalculatedAt = &daily
		state.FullMonths = 1
		if _, err := upsertState(ctx, pool, user, version+1, stateRowOf(state)); err != nil {
			t.Fatal(err)
		}
		check(false, true)
	}
}
