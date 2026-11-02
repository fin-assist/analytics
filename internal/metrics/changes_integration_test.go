package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationChangesAtomicMarkerAndImmutableFinancialSnapshot(t *testing.T) {
	e := start(t)
	c, err := metrics.NewChanges(e.db, e.stones, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	user := userID(51)
	ctx := t.Context()
	u := domain.Upsert{UserID: user, Version: 4, State: state(4), Snapshots: []domain.Snapshot{monthSnapshot("2026-09", domain.TransferWith, 4, "tiles.expense")}}
	if _, err = e.uc.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	beforeHeaders, beforeMetrics, beforeVersion := rows(t, e, user)
	count := func() int {
		t.Helper()
		var n int
		if err := e.db.Primary().QueryRow(ctx, `SELECT count(*) FROM recalculation_marker WHERE user_id=$1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	boom := errors.New("composition failed")
	if err = c.Apply(ctx, user, 5, func(context.Context) error { return boom }); !errors.Is(err, boom) || count() != 0 {
		t.Fatal("failed local composition committed marker", err)
	}
	if err = c.Apply(ctx, user, 5, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	h, m, v := rows(t, e, user)
	if h != beforeHeaders || m != beforeMetrics || v != beforeVersion {
		t.Fatal("financial state changed", h, m, v)
	}
	got, err := e.repo.ReadAnalytics(ctx, user, domain.PeriodMonth, nil, domain.TransferWith)
	if err != nil || !got.Recalculating || got.AppliedVersion != 4 {
		t.Fatal(got, err)
	}
	// The existing final publication, not Apply, advances the financial snapshot version.
	u.Version = 6
	u.Snapshots = nil
	if _, err = e.uc.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(ctx, user, 5, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err = e.repo.ReadAnalytics(ctx, user, domain.PeriodMonth, nil, domain.TransferWith)
	if err != nil || got.Recalculating || got.AppliedVersion != 6 {
		t.Fatal("late target changed final", got, err)
	}
	if err = c.Mark(ctx, user, 9); err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(ctx, user, 5, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err = e.repo.ReadAnalytics(ctx, user, domain.PeriodMonth, nil, domain.TransferWith)
	if err != nil || !got.Recalculating {
		t.Fatal("old target cleared new marker", got, err)
	}
	if err = e.db.Run(ctx, func(ctx context.Context) error { return e.stones.Purge(ctx, user, e.repo.Purge) }); err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(ctx, user, 10, func(context.Context) error { t.Fatal("deleted user composed"); return nil }); !errors.Is(err, metrics.ErrDeleted) {
		t.Fatal(err)
	}
	if err = c.Mark(ctx, user, 10); err != nil || count() != 0 {
		t.Fatal("deleted marker revived", err)
	}
}
