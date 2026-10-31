package postgres

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationForecastHistoryReadsOneTenantInventoryAndDeliveryProof(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	rows, state, _ := historyFixture(t)
	user := newUser(401)
	writeState := func(version int32) {
		t.Helper()
		row := stateRowOf(state)
		if _, err := upsertState(ctx, pool, user, version, row); err != nil {
			t.Fatal(err)
		}
	}
	var snapshots []domain.Snapshot
	for _, row := range rows {
		for _, mode := range domain.TransferModes {
			snapshots = append(snapshots, domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: row.Key}, Mode: mode}, Range: &row.Range, Coverage: row.Coverage, DataFrom: row.DataFrom, DataTo: row.DataTo, Gaps: row.Gaps, CalculatedAt: state.CalculatedAt, Revisions: state.Revisions})
		}
	}
	gapStart := time.Date(2026, 10, 10, 0, 0, 0, 0, rows[12].Range.Start.Location())
	gapEnd := gapStart.AddDate(0, 0, 3)
	for i := range snapshots {
		if snapshots[i].Key.Key == "2026-10" {
			snapshots[i].Gaps = []domain.TimeRange{{Start: &gapStart, End: &gapEnd}}
		}
	}
	if _, err := upsertPeriods(ctx, pool, user, 4, snapshots, nil); err != nil {
		t.Fatal(err)
	}
	writeState(4)
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	read := func() *domain.ForecastHistory {
		t.Helper()
		key := "2026-10"
		got, err := repo.ReadAnalytics(ctx, user, domain.PeriodMonth, &key, domain.TransferWith)
		if err != nil {
			t.Fatal(err)
		}
		return got.History
	}
	if got := read(); got == nil || len(got.Months) != 13 {
		t.Fatalf("history %+v", got)
	}
	modeKey := "2026-10"
	without, err := repo.ReadAnalytics(ctx, user, domain.PeriodMonth, &modeKey, domain.TransferWithout)
	if err != nil || without.History == nil || len(without.History.Months) != 13 {
		t.Fatalf("without mode %+v %v", without, err)
	}
	gotHistory := read()
	if len(gotHistory.Months[12].Gaps) != 1 || !gotHistory.Months[12].Gaps[0].Start.Equal(gapStart) || !gotHistory.Months[12].Gaps[0].End.Equal(gapEnd) {
		t.Fatal("gap JSON was lost", gotHistory)
	}
	clock := *state.CalculatedAt
	service, err := usecase.NewScreen(repo, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	readScreen := func() *domain.ForecastHistory {
		t.Helper()
		key := "2026-10"
		got, err := service.Get(ctx, user, domain.PeriodMonth, &key, domain.TransferWith)
		if err != nil {
			t.Fatal(err)
		}
		return got.History
	}
	if readScreen() == nil {
		t.Fatal("fresh source inventory was lost")
	}
	clock = *rows[12].Range.End
	if readScreen() != nil {
		t.Fatal("month rollover used yesterday's basis before daily delivery")
	}

	// Another tenant's newer header must not invalidate this user's completed inventory.
	if _, err := upsertPeriods(ctx, pool, newUser(402), 100, snapshots[:1], nil); err != nil {
		t.Fatal(err)
	}
	if read() == nil {
		t.Fatal("foreign history invalidated this tenant")
	}
	state.Recalculating = true
	writeState(5)
	if read() != nil {
		t.Fatal("in-progress delivery fabricated a basis")
	}
	state.Recalculating = false
	writeState(6)
	if read() == nil {
		t.Fatal("unchanged source snapshots with older calc_version were discarded")
	}
	// The producer's daily job updates the current state but can retain last month's PARTIAL header.
	end := *rows[12].Range.End
	last := end.Add(-time.Second)
	now := end.Add(12 * time.Hour)
	var octoberSnapshots []domain.Snapshot
	for _, snapshot := range snapshots {
		if snapshot.Key.Key == "2026-10" {
			snapshot.DataTo = &last
			snapshot.Gaps = nil
			octoberSnapshots = append(octoberSnapshots, snapshot)
		}
	}
	if _, err := upsertPeriods(ctx, pool, user, 7, octoberSnapshots, nil); err != nil {
		t.Fatal(err)
	}
	state.DataTo = &end
	state.CalculatedAt = &now
	state.FullMonths = 12
	writeState(7)
	if readScreen() == nil {
		t.Fatal("fresh daily delivery did not restore history")
	}
	if got := read(); got == nil || got.Months[12].Coverage != domain.CoverageComplete {
		t.Fatalf("rollover history %+v", got)
	}
	if _, err := upsertPeriods(ctx, pool, user, 8, nil, []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2025-11"}}); err != nil {
		t.Fatal(err)
	}
	if read() != nil {
		t.Fatal("newer removal did not invalidate completion proof")
	}
	writeState(9)
	if read() != nil {
		t.Fatal("missing full snapshot did not invalidate the count")
	}
}

func TestIntegrationKnownEarlyNoDataUsesCompletedGlobalInventory(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	rows, state, _ := historyFixture(t)
	user := newUser(403)
	row := rows[len(rows)-1]
	state.DataFrom = row.DataFrom
	state.LastOperationAt = row.DataFrom
	state.FullMonths = 0
	snapshots := []domain.Snapshot{}
	for _, mode := range domain.TransferModes {
		snapshots = append(snapshots, domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: row.Key}, Mode: mode}, Range: &row.Range, Coverage: domain.CoveragePartial, DataFrom: row.DataFrom, DataTo: row.DataTo, CalculatedAt: state.CalculatedAt, Revisions: state.Revisions})
	}
	if _, err := upsertPeriods(ctx, pool, user, 4, snapshots, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertState(ctx, pool, user, 4, stateRowOf(state)); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	screen, err := usecase.NewScreen(repo, func() time.Time { return *state.CalculatedAt })
	if err != nil {
		t.Fatal(err)
	}
	key := "2026-09"
	got, err := screen.Get(ctx, user, domain.PeriodMonth, &key, domain.TransferWithout)
	if err != nil {
		t.Fatal(err)
	}
	composer, err := metricsgrpc.NewComposer(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	response, err := composer.Compose(ctx, got, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Navigation.GetNext() != "" || response.Navigation.GetPrevious() != "" {
		t.Fatal(response.Navigation)
	}
	for _, block := range response.Blocks {
		if block.Id == "monthly_chart" {
			t.Fatal("known early inventory fabricated a chart")
		}
		if block.Id == "tiles" && (block.Status != "none" || len(block.Data.AsMap()["tiles"].([]any)) != 0) {
			t.Fatal(block)
		}
	}
}
