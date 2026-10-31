package postgres

import (
	"os"
	"reflect"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestIntegrationRollingSlicesRoundTripReplaceAndTombstones(t *testing.T) {
	pool := openDB(t)
	ctx := t.Context()
	user := newUser(411)
	bytes, err := os.ReadFile("../../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err := protojson.Unmarshal(bytes, req); err != nil {
		t.Fatal(err)
	}
	state := validState()
	state.dataFrom = ptr(req.State.DataFrom.AsTime())
	state.dataTo = ptr(req.State.DataTo.AsTime())
	state.lastOperation = ptr(req.State.LastOperationAt.AsTime())
	state.calculatedAt = ptr(req.State.CalculatedAt.AsTime())
	if _, err := upsertState(ctx, pool, user, 4, state); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertProto(ctx, pool, user, 4, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	check := func() {
		t.Helper()
		for _, expected := range req.Periods {
			kind := domain.PeriodType(expected.Period)
			mode := domain.TransferMode(expected.TransferMode)
			got, err := repo.ReadAnalytics(ctx, user, kind, &expected.PeriodKey, mode)
			if err != nil {
				t.Fatal(err)
			}
			if got.Snapshot == nil || got.Snapshot.Key.Mode != mode || !got.Snapshot.Range.Start.Equal(expected.Range.Start.AsTime()) || !got.Snapshot.Range.End.Equal(expected.Range.End.AsTime()) {
				t.Fatalf("snapshot boundaries %v", got.Snapshot)
			}
			for _, stored := range got.Snapshot.Metrics {
				if stored.ID != "monthly_chart" {
					continue
				}
				holder := &analyticsv1.Metric{}
				if err := protojson.Unmarshal(stored.Details, holder); err != nil {
					t.Fatal(err)
				}
				var chart *analyticsv1.MonthlyChart
				for _, m := range expected.Metrics {
					if m.MetricId == "monthly_chart" {
						chart = m.Metric.GetMonthlyChart()
					}
				}
				if !proto.Equal(chart, holder.GetMonthlyChart()) {
					t.Fatal("storage lost calculation_range or converted unknown month to zero")
				}
			}
			listed, err := repo.ListPeriods(ctx, user, kind)
			if err != nil || len(listed.Items) != 1 || listed.Items[0].Key != expected.PeriodKey {
				t.Fatalf("singleton list %v %v", listed, err)
			}
		}
	}
	check()
	// Replacing the pair keeps a single row per mode and replaces the typed financial payload.
	for _, period := range req.Periods {
		for _, m := range period.Metrics {
			if m.MetricId == "monthly_chart" {
				m.Metric.GetMonthlyChart().Points[len(m.Metric.GetMonthlyChart().Points)-1].ExpenseMinor = proto.Int64(1234)
			}
		}
	}
	if _, err := upsertProto(ctx, pool, user, 5, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	check()
	removed := []*analyticsv1.PeriodRef{{Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_YEAR, PeriodKey: "rolling_year"}, {Period: commonv1.PeriodType_PERIOD_TYPE_ROLLING_QUARTER, PeriodKey: "rolling_quarter"}}
	if _, err := upsertProto(ctx, pool, user, 6, nil, removed); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertProto(ctx, pool, user, 5, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []domain.PeriodType{domain.PeriodRollingYear, domain.PeriodRollingQuarter} {
		key := kind.String()
		got, err := repo.ReadAnalytics(ctx, user, kind, &key, domain.TransferWith)
		if err != nil || got.Snapshot != nil {
			t.Fatalf("outbox resurrected singleton %v %v", got.Snapshot, err)
		}
	}
}

func TestIntegrationRollingListUsesAvailableWithoutHeader(t *testing.T) {
	pool := openDB(t)
	user := newUser(412)
	raw, err := os.ReadFile("../../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	state := validState()
	state.dataFrom = ptr(req.State.DataFrom.AsTime())
	state.dataTo = ptr(req.State.DataTo.AsTime())
	state.lastOperation = ptr(req.State.LastOperationAt.AsTime())
	state.calculatedAt = ptr(req.State.CalculatedAt.AsTime())
	if _, err = upsertState(t.Context(), pool, user, 4, state); err != nil {
		t.Fatal(err)
	}
	var without []*analyticsv1.PeriodSnapshot
	for _, p := range req.Periods {
		if p.TransferMode == commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
			p.Bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
			p.Bounds.Gaps = nil
			p.Metrics = nil
			without = append(without, p)
		}
	}
	if _, err = upsertProto(t.Context(), pool, user, 4, without, nil); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	for _, p := range without {
		got, err := repo.ListPeriods(t.Context(), user, domain.PeriodType(p.Period))
		if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != domain.CoverageComplete || !got.Items[0].Start.Equal(p.Range.Start.AsTime()) || !got.Items[0].End.Equal(p.Range.End.AsTime()) {
			t.Fatalf("WITHOUT-only header lost: %+v %v", got, err)
		}
	}
	// With both headers present, preserve the existing list's WITHOUT-first selection.
	var with []*analyticsv1.PeriodSnapshot
	for _, p := range req.Periods {
		if p.TransferMode == commonv1.TransferMode_TRANSFER_MODE_WITH {
			with = append(with, p)
		}
	}
	if _, err = upsertProto(t.Context(), pool, user, 4, with, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range without {
		got, err := repo.ListPeriods(t.Context(), user, domain.PeriodType(p.Period))
		if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != domain.CoverageComplete {
			t.Fatalf("mode precedence changed: %+v %v", got, err)
		}
	}
	// A removed WITHOUT marker must not hide the still available WITH header.
	if _, err = pool.Exec(t.Context(), `UPDATE period_snapshot SET removed=true WHERE user_id=$1 AND transfer_mode=2`, user); err != nil {
		t.Fatal(err)
	}
	for _, p := range with {
		got, err := repo.ListPeriods(t.Context(), user, domain.PeriodType(p.Period))
		if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != domain.CoveragePartial {
			t.Fatalf("WITH fallback lost: %+v %v", got, err)
		}
	}

}

func TestIntegrationRollingListCertifiesOnlyCurrentHeaderRevisions(t *testing.T) {
	pool := openDB(t)
	user := newUser(413)
	raw, err := os.ReadFile("../../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	state := validState()
	state.dataFrom = ptr(req.State.DataFrom.AsTime())
	state.dataTo = ptr(req.State.DataTo.AsTime())
	state.lastOperation = ptr(req.State.LastOperationAt.AsTime())
	state.calculatedAt = ptr(req.State.CalculatedAt.AsTime())
	state.revisions = revisionsJSON(domain.Revisions{"uploads": 2, "categories": 1, "time_zone": 1})
	if _, err = upsertState(t.Context(), pool, user, 4, state); err != nil {
		t.Fatal(err)
	}
	for _, p := range req.Periods {
		p.Bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
		p.Bounds.Gaps = nil
		p.Metrics = nil
	}
	if _, err = upsertProto(t.Context(), pool, user, 4, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(&DB{primary: pool, replica: pool})
	check := func(coverage domain.Coverage, revisions domain.Revisions) {
		t.Helper()
		for _, typ := range []domain.PeriodType{domain.PeriodRollingYear, domain.PeriodRollingQuarter} {
			got, err := repo.ListPeriods(t.Context(), user, typ)
			if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != coverage || !reflect.DeepEqual(got.Revisions, revisions) {
				t.Fatalf("metadata %+v %v, want %v %v", got, err, coverage, revisions)
			}
		}
	}
	check(domain.CoverageComplete, domain.Revisions{"categories": 1, "time_zone": 1})
	next := state.dataTo.AddDate(0, 0, 1)
	state.dataTo = &next
	if _, err = upsertState(t.Context(), pool, user, 5, state); err != nil {
		t.Fatal(err)
	}
	check(domain.CoveragePartial, domain.Revisions{"uploads": 2, "categories": 1, "time_zone": 1})
	removed := []*analyticsv1.PeriodRef{{Period: 5, PeriodKey: "rolling_year"}, {Period: 6, PeriodKey: "rolling_quarter"}}
	if _, err = upsertProto(t.Context(), pool, user, 6, nil, removed); err != nil {
		t.Fatal(err)
	}
	if _, err = upsertState(t.Context(), pool, user, 6, state); err != nil {
		t.Fatal(err)
	}
	check(domain.CoveragePartial, domain.Revisions{"uploads": 2, "categories": 1, "time_zone": 1})
}
