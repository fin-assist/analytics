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
	user := newUser(405)
	state.DataFrom = row.DataFrom
	state.LastOperationAt = row.DataFrom
	state.FullMonths = 0
	periods := []struct {
		typ           domain.PeriodType
		key           string
		month, months int
	}{{domain.PeriodMonth, "2026-10", 10, 1}, {domain.PeriodQuarter, "2026-Q4", 10, 3}, {domain.PeriodYear, "2026", 1, 12}}
	snapshots := []domain.Snapshot{}
	for _, period := range periods {
		start := time.Date(2026, time.Month(period.month), 1, 0, 0, 0, 0, row.Range.Start.Location())
		end := start.AddDate(0, period.months, 0)
		for _, mode := range domain.TransferModes {
			snapshots = append(snapshots, domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: period.typ, Key: period.key}, Mode: mode}, Range: &domain.TimeRange{Start: &start, End: &end}, Coverage: domain.CoveragePartial, DataFrom: row.DataFrom, DataTo: row.DataTo, CalculatedAt: state.CalculatedAt, Revisions: state.Revisions,
				Metrics: []domain.Metric{{ID: "monthly_chart", DefinitionVersion: 1, Status: 3, Lock: &domain.Lock{Reason: 2, Required: ptr(int32(2)), Available: ptr(int32(1))}}}})
		}
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
	for _, known := range []bool{true, false} {
		version := int32(4)
		if !known {
			version = 5
			state.Revisions = nil
			for i := range snapshots {
				snapshots[i].Revisions = nil
			}
		}
		if _, err := upsertPeriods(ctx, pool, user, version, snapshots, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := upsertState(ctx, pool, user, version, stateRowOf(state)); err != nil {
			t.Fatal(err)
		}
		for _, period := range periods {
			got, err := screen.Get(ctx, user, period.typ, &period.key, domain.TransferWithout)
			if err != nil {
				t.Fatal(err)
			}
			if (got.History != nil) != known {
				t.Fatalf("fixture history for %s known=%v: %+v", period.key, known, got.History)
			}
			response, err := composer.Compose(ctx, got, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, block := range response.Blocks {
				if block.Id == "monthly_chart" {
					t.Fatalf("period %s known=%v fabricated chart", period.key, known)
				}
			}
		}
	}
}
