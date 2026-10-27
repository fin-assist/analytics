package app_test

import (
	"context"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

// The signature spans independently read relations and the implicit period selection.
// Every replacement changes the latest key, its neighbour, state and metrics atomically.
func concurrentSnapshot(t *testing.T, version int32) *analyticsv1.UpsertUserMetricsRequest {
	t.Helper()
	r := analyticsFixture(t)
	r.CalcVersion = version
	selected, previous := "2026-09", "2026-07"
	removed := []string{"2026-10", "2026-08"}
	if version%2 == 0 {
		selected, previous = "2026-10", "2026-08"
		removed = []string{"2026-09", "2026-07"}
	}
	r.State.FullMonths = 3 + (version+1)%2
	r.State.Recalculating = version%2 == 0
	stamp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(version) * time.Second)
	r.State.CalculatedAt = timestamppb.New(stamp)
	r.State.LastOperationAt = timestamppb.New(stamp)
	r.State.DataTo = at(monthStart(selected).AddDate(0, 1, 0))
	r.State.StaleAfter = at(monthStart(selected).AddDate(0, 1, 14))
	base := r.Periods
	r.Periods = nil
	for _, key := range []string{selected, previous} {
		for _, s := range base {
			copy := proto.Clone(s).(*analyticsv1.PeriodSnapshot)
			start := monthStart(key)
			end := start.AddDate(0, 1, 0)
			copy.PeriodKey = key
			copy.Range = &commonv1.TimeRange{Start: at(start), End: at(end)}
			copy.Bounds = &commonv1.DataBounds{Coverage: 1, DataFrom: at(start), DataTo: at(end.Add(-time.Second))}
			copy.CalculatedAt = timestamppb.New(stamp)
			for _, metric := range copy.Metrics {
				if metric.MetricId == "tiles.expense" {
					metric.Metric.ValueMinor = proto.Int64(int64(version) * 100)
				}
			}
			r.Periods = append(r.Periods, copy)
		}
	}
	for _, key := range removed {
		r.RemovedPeriods = append(r.RemovedPeriods, &analyticsv1.PeriodRef{Period: 1, PeriodKey: key})
	}
	return r
}

func assertConcurrentScreen(t *testing.T, r *analyticsv1.GetAnalyticsResponse) {
	t.Helper()
	version := int32(r.GetState().GetCalculatedAt().AsTime().Sub(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) / time.Second)
	key, previous := "2026-09", "2026-07"
	if version%2 == 0 {
		key, previous = "2026-10", "2026-08"
	}
	if !r.HasData || r.Params.GetPeriodKey() != key || r.Navigation.GetPrevious() != previous || r.Navigation.Next != nil || r.State.Recalculating != (version%2 == 0) || r.State.FullMonths != 3+(version+1)%2 {
		t.Fatalf("mixed default/state/navigation: %+v", r)
	}
	b := screenBlock(r, "expense_categories")
	if b == nil || b.Status != "ready" {
		t.Fatalf("missing selected snapshot %+v", r)
	}
	amount := b.Data.AsMap()["total"].(map[string]any)["value"]
	if amount != float64(version)*100 {
		t.Fatalf("mixed metric version: state=%d, amount=%v", version, amount)
	}
	stamp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(version) * time.Second)
	if !r.State.CalculatedAt.AsTime().Equal(stamp) || !r.State.LastOperationAt.AsTime().Equal(stamp) || !r.Period.Range.Start.AsTime().Equal(monthStart(key)) {
		t.Fatalf("mixed snapshot header %+v", r)
	}
}

func TestAPIGetAnalyticsDefaultIsOneSnapshotDuringConcurrentReplacement(t *testing.T) {
	env, c := startAPI(t)
	env.App.Now = func() time.Time { return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC) }
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), concurrentSnapshot(t, 1)); err != nil {
		t.Fatal(err)
	}
	// Prepare immutable requests before starting the writer: no test helper is called by it.
	updates := make([]*analyticsv1.UpsertUserMetricsRequest, 64)
	for i := range updates {
		updates[i] = concurrentSnapshot(t, int32(i+2))
	}
	next := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for _, req := range updates {
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-next:
			}
			_, err := c.ingest.UpsertUserMetrics(producer(ctx), req)
			done <- err
			if err != nil {
				return
			}
		}
	}()
	for i := range updates {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case next <- struct{}{}:
		}
		for range 2 {
			r, err := c.analytics.GetAnalytics(apitest.User(ctx, alice), &analyticsv1.GetAnalyticsRequest{Supports: []*commonv1.BlockSupport{{Type: "ranked_list", MaxVersion: 1}}})
			if err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
			assertConcurrentScreen(t, r)
			if version := int32(r.State.CalculatedAt.AsTime().Sub(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) / time.Second); version < int32(i+1) || version > int32(i+2) {
				t.Fatalf("unexpected committed version %d in round %d", version, i)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
