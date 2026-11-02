package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/analytics/internal/domain"
)

// The repository now owns coherent read transactions, including its paged projection.
func testMetricsDB(pool *pgxpool.Pool) *DB {
	return &DB{primary: pool, replica: pool, transactions: pgstore.New(pool)}
}

func seedChart(t *testing.T, pool *pgxpool.Pool, user string, version int32, count int) {
	t.Helper()
	zone, _ := time.LoadLocation("Europe/Moscow")
	first := time.Date(2010, 1, 1, 0, 0, 0, 0, zone)
	end := first.AddDate(0, count, 0)
	state := validState()
	state.dataFrom = &first
	state.dataTo = &end
	state.lastOperation = ptr(end.Add(-time.Second))
	state.calculatedAt = &end
	if _, err := upsertState(t.Context(), pool, user, version, state); err != nil {
		t.Fatal(err)
	}
	points := make([]any, 0, count)
	for i := range count {
		start := first.AddDate(0, i, 0)
		next := start.AddDate(0, 1, 0)
		points = append(points, map[string]any{"month": start.Format("2006-01"), "range": map[string]any{"start": start, "end": next}, "expense_minor": i, "income_minor": i + 1, "bounds": map[string]any{"coverage": 1}})
	}
	raw, err := json.Marshal(map[string]any{"monthly_chart": map[string]any{"points": points}})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodAllTime, Key: "all_time"}, Mode: domain.TransferWith}, TimeZone: "Europe/Moscow", Range: &domain.TimeRange{Start: &first, End: &end}, Coverage: domain.CoverageComplete, DataFrom: &first, DataTo: ptr(end.Add(-time.Second)), CalculatedAt: &end,
		Metrics: []domain.Metric{{ID: "monthly_chart", DefinitionVersion: 1, Status: 1, Details: raw}}}
	if _, err = upsertPeriods(t.Context(), pool, user, version, []domain.Snapshot{snap}, nil); err != nil {
		t.Fatal(err)
	}
}

func pageMonths(t *testing.T, page *domain.ChartPage) []string {
	t.Helper()
	var decoded struct {
		Chart struct{ Points []struct{ Month string } } `json:"monthly_chart"`
	}
	if page == nil {
		t.Fatal("missing chart")
	}
	if err := json.Unmarshal(page.Details, &decoded); err != nil {
		t.Fatal(err)
	}
	months := make([]string, len(decoded.Chart.Points))
	for i, p := range decoded.Chart.Points {
		months[i] = p.Month
	}
	return months
}

func TestIntegrationChartPagesBoundedTenantRevisionAndClock(t *testing.T) {
	pool := openDB(t)
	user, other := newUser(416), newUser(417)
	seedChart(t, pool, user, 1, 121)
	seedChart(t, pool, other, 1, 121)
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	repo := NewMetrics(testMetricsDB(pool), func() time.Time { return clock })
	key := "all_time"
	first, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith)
	if err != nil {
		t.Fatal(err)
	}
	months := pageMonths(t, first.Chart)
	if len(months) != 60 || months[0] != "2015-02" || months[59] != "2020-01" || first.Chart.Next != "" || first.Chart.Previous == "" {
		t.Fatal(months, first.Chart)
	}
	second, err := repo.ReadChartPage(t.Context(), user, first.Chart.Previous)
	if err != nil {
		t.Fatal(err)
	}
	months = pageMonths(t, second)
	if len(months) != 60 || months[0] != "2010-02" || months[59] != "2015-01" || second.Revision != first.Chart.Revision || second.Next == "" || second.Previous == "" {
		t.Fatal(months, second)
	}
	oldest, err := repo.ReadChartPage(t.Context(), user, second.Previous)
	if err != nil {
		t.Fatal(err)
	}
	months = pageMonths(t, oldest)
	if len(months) != 1 || months[0] != "2010-01" || oldest.Previous != "" {
		t.Fatal(months, oldest)
	}
	back, err := repo.ReadChartPage(t.Context(), user, oldest.Next)
	if err != nil || back.Revision != second.Revision || back.Previous != second.Previous {
		t.Fatalf("reverse page %v %v", back, err)
	}
	if _, err = repo.ReadChartPage(t.Context(), other, first.Chart.Previous); !errors.Is(err, domain.ErrCursorFormat) {
		t.Fatal("foreign cursor", err)
	}
	for _, bad := range []string{"", "bad", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"} {
		if _, err = repo.ReadChartPage(t.Context(), user, bad); !errors.Is(err, domain.ErrCursorFormat) {
			t.Fatal("invalid cursor", err)
		}
	}
	selected := "2010-01"
	oldSelection, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodMonth, &selected, domain.TransferWith)
	if err != nil || oldSelection.Chart.Revision != first.Chart.Revision || len(pageMonths(t, oldSelection.Chart)) != 1 {
		t.Fatalf("period reuse %v %v", oldSelection.Chart, err)
	}
	clock = clock.AddDate(0, 0, 1)
	if _, err = repo.ReadChartPage(t.Context(), user, first.Chart.Previous); !errors.Is(err, domain.ErrCursorRange) {
		t.Fatal("daily stale cursor", err)
	}
	fresh, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith)
	if err != nil || fresh.Chart.Revision == first.Chart.Revision {
		t.Fatalf("fresh revision %v %v", fresh.Chart, err)
	}
	seedChart(t, pool, user, 2, 122)
	if _, err = repo.ReadChartPage(t.Context(), user, fresh.Chart.Previous); !errors.Is(err, domain.ErrCursorRange) {
		t.Fatal("changed snapshot cursor", err)
	}
	if err = repo.db.Run(t.Context(), func(ctx context.Context) error { return repo.Purge(ctx, user) }); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = pool.QueryRow(t.Context(), "SELECT count(*) FROM chart_cursor WHERE user_id=$1", user).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("cursor leaked after purge", rows, err)
	}
}

func TestIntegrationChartReadsUsePrimaryEvenWhenReplicaHasOlderSource(t *testing.T) {
	primary, replica := openDB(t), openDB(t)
	user := newUser(418)
	seedChart(t, primary, user, 2, 121)
	seedChart(t, replica, user, 1, 2)
	db := testMetricsDB(primary)
	db.replica = replica
	repo := NewMetrics(db, func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) })
	key := "all_time"
	first, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith)
	if err != nil || len(pageMonths(t, first.Chart)) != 60 || first.AppliedVersion != 2 {
		t.Fatalf("read used stale replica %v %v", first, err)
	}
	next, err := repo.ReadChartPage(t.Context(), user, first.Chart.Previous)
	if err != nil || next.Revision != first.Chart.Revision || len(pageMonths(t, next)) != 60 {
		t.Fatalf("fresh cursor rejected by stale replica %v %v", next, err)
	}
}

func TestIntegrationAllTimePeakUsesEarliestEqualExpense(t *testing.T) {
	pool := openDB(t)
	user := newUser(419)
	seedChart(t, pool, user, 1, 121)
	if _, err := pool.Exec(t.Context(), `UPDATE period_metric SET details=jsonb_set(details,'{monthly_chart,points}',(SELECT jsonb_agg(jsonb_set(point,'{expense_minor}','"0"'::jsonb) ORDER BY ordinal) FROM jsonb_array_elements(details->'monthly_chart'->'points') WITH ORDINALITY AS p(point,ordinal))) WHERE user_id=$1 AND metric_id='monthly_chart'`, user); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(testMetricsDB(pool), time.Now)
	key := "all_time"
	got, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith)
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Chart struct{ Points []struct{ Month string } } `json:"monthly_chart"`
	}
	for _, m := range got.Snapshot.Metrics {
		if m.ID == "monthly_chart" {
			if err = json.Unmarshal(m.Details, &detail); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(detail.Chart.Points) != 1 || detail.Chart.Points[0].Month != "2010-01" {
		t.Fatal(detail)
	}
}
