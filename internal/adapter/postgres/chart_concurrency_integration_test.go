package postgres

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/analytics/internal/domain"
)

// Both readers have already established their primary snapshot before cursor issuance starts.
type chartSnapshotBarrier struct {
	arrived chan struct{}
	release chan struct{}
	heads   atomic.Int32
}

func (b *chartSnapshotBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if q.SQL == chartHeadSQL && b.heads.Add(1) <= 2 {
		b.arrived <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*chartSnapshotBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestIntegrationChartConcurrentReadersRetryWholeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		page, existing, expired bool
	}{
		{name: "initial screen"}, {name: "existing screen", existing: true},
		{name: "initial page", page: true}, {name: "existing page", page: true, existing: true},
		{name: "concurrent expiry cleanup", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := openDB(t)
			user := newUser(418)
			seedChart(t, pool, user, 1, 121)
			now := func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
			repo := NewMetrics(testMetricsDB(pool), now)
			key := "all_time"
			cursor := ""
			if tc.page || tc.existing {
				initial, err := repo.ReadAnalytics(t.Context(), user, domain.PeriodAllTime, &key, domain.TransferWith)
				if err != nil {
					t.Fatal(err)
				}
				cursor = initial.Chart.Previous
				if tc.page && tc.existing {
					if _, err = repo.ReadChartPage(t.Context(), user, cursor); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.expired {
				if _, err := pool.Exec(t.Context(), `INSERT INTO chart_cursor(user_id,transfer_mode,revision,page,token,created_at) VALUES($1,1,'expired',0,'expired',now()-interval '2 days')`, user); err != nil {
					t.Fatal(err)
				}
			}
			var before string
			if tc.existing {
				if err := pool.QueryRow(t.Context(), `SELECT string_agg(token||xmin::text,',' ORDER BY token) FROM chart_cursor WHERE user_id=$1`, user).Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			barrier := &chartSnapshotBarrier{arrived: make(chan struct{}, 2), release: make(chan struct{})}
			config := pool.Config()
			config.ConnConfig.Tracer = barrier
			traced, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer traced.Close()
			repo = NewMetrics(testMetricsDB(traced), now)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type result struct {
				page *domain.ChartPage
				err  error
			}
			done := make(chan result, 2)
			for range 2 {
				go func() {
					if tc.page {
						p, e := repo.ReadChartPage(ctx, user, cursor)
						done <- result{p, e}
						return
					}
					a, e := repo.ReadAnalytics(ctx, user, domain.PeriodAllTime, &key, domain.TransferWith)
					done <- result{a.Chart, e}
				}()
			}
			for range 2 {
				select {
				case <-barrier.arrived:
				case <-ctx.Done():
					close(barrier.release)
					t.Fatal("readers did not reach established snapshots", ctx.Err())
				}
			}
			close(barrier.release)
			var first *domain.ChartPage
			for range 2 {
				select {
				case got := <-done:
					if got.err != nil || got.page == nil {
						t.Fatalf("concurrent read failed: %v", got.err)
					}
					if first == nil {
						first = got.page
					} else if first.Revision != got.page.Revision || first.Previous != got.page.Previous || first.Next != got.page.Next {
						t.Fatal("mixed or duplicate pointers", first, got.page)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if tc.existing {
				var after string
				if err := pool.QueryRow(t.Context(), `SELECT string_agg(token||xmin::text,',' ORDER BY token) FROM chart_cursor WHERE user_id=$1`, user).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if before != after || barrier.heads.Load() != 2 {
					t.Fatal("existing pointers were rewritten", before, after, barrier.heads.Load())
				}
			} else if barrier.heads.Load() < 3 {
				t.Fatal("test did not exercise serialization retry")
			}
		})
	}
}
