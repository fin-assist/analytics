package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func analyticsRow() []any {
	key := "2026-09"
	previous, next := "2026-08", "2026-10"
	id := "tiles.expense"
	version := int32(1)
	status, coverage, reason, kind, mode := int16(1), int16(2), int16(1), int16(1), int16(2)
	required, available, months := int32(3), int32(1), int32(3)
	amount := int64(0)
	category, selection, name := "cat_refund", "s_fees", "Комиссии"
	return []any{&key, true, instant(1), instant(20), instant(20), instant(30), int32(3), true, "Europe/Moscow", instant(21),
		true, instant(1), instant(30), &coverage, instant(1), instant(20), &months, instant(21), []time.Time{*instant(3)}, []time.Time{*instant(4)}, &previous, &next, []byte(nil), []byte(nil), &version, &version, []byte(nil),
		&id, &version, &status, &amount, instant(1), instant(20), &reason, &required, &available, instant(10), instant(1), instant(20), &months,
		instant(1), instant(30), &category, &kind, &mode, &selection, &name, []byte(`{"bank_fees":{}}`)}
}

func TestUnitReadAnalyticsRowsAndRepository(t *testing.T) {
	row := analyticsRow()
	tx := &scriptedTx{rows: map[string]*scriptedRows{"unknown": {values: [][]any{row}}}}
	repo, db := unitRepository(tx)
	var got domain.AnalyticsData
	err := db.Run(t.Context(), func(ctx context.Context) error {
		var e error
		got, e = repo.ReadAnalytics(ctx, "user", domain.PeriodMonth, nil, domain.TransferWithout)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.State == nil || !got.State.Recalculating || got.Snapshot.Key.Key != "2026-09" || got.Snapshot.Key.Mode != domain.TransferWithout || *got.Previous != "2026-08" || got.Snapshot.UnreadLines != 3 || got.Snapshot.Coverage != domain.CoveragePartial {
		t.Fatalf("read %+v", got)
	}
	m := got.Snapshot.Metrics[0]
	if m.ValueMinor == nil || *m.ValueMinor != 0 || m.Lock == nil || *m.Lock.Available != 1 || m.Basis == nil || m.Basis.FullMonths != 3 || m.Filter == nil || *m.Filter.Selection != "s_fees" || m.Filter.TransferMode != 2 || m.Range == nil {
		t.Fatalf("metric %+v", m)
	}
	if string(m.Details) != `{"bank_fees":{}}` {
		t.Fatal(string(m.Details))
	}
	row = analyticsRow()
	row[10] = false
	for i := 11; i < len(row); i++ {
		row[i] = nil
	}
	tx.rows["unknown"] = &scriptedRows{values: [][]any{row}}
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		var e error
		key := "2026-07"
		got, e = repo.ReadAnalytics(ctx, "user", domain.PeriodMonth, &key, domain.TransferWith)
		return e
	}); err != nil || got.Snapshot != nil {
		t.Fatalf("missing snapshot %+v %v", got, err)
	}
	tx.rows["unknown"] = &scriptedRows{}
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		var e error
		got, e = repo.ReadAnalytics(ctx, "user", domain.PeriodMonth, nil, domain.TransferWith)
		return e
	}); err != nil || got.State != nil {
		t.Fatalf("absent state %+v %v", got, err)
	}
}

func TestUnitReadAnalyticsFailures(t *testing.T) {
	boom := errors.New("read failed")
	for _, tc := range []struct {
		name  string
		rows  *scriptedRows
		query error
		want  error
	}{
		{"query", nil, boom, boom}, {"scan", &scriptedRows{values: [][]any{analyticsRow()}, scanErr: boom}, nil, boom},
		{"iteration", &scriptedRows{err: boom}, nil, boom}, {"absent", &scriptedRows{}, nil, errNoState},
		{"header", &scriptedRows{values: [][]any{func() []any { r := analyticsRow(); r[11] = nil; return r }()}}, nil, errBrokenHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &scriptedTx{rows: map[string]*scriptedRows{"unknown": tc.rows}, queryErr: map[string]error{"unknown": tc.query}}
			if _, err := readAnalyticsRows(t.Context(), tx, "u", pk(month, "2026-09", withTransfers), nil); !errors.Is(err, tc.want) {
				t.Fatalf("%v want %v", err, tc.want)
			}
			if errors.Is(tc.want, errNoState) {
				return
			}
			repo, db := unitRepository(&scriptedTx{rows: map[string]*scriptedRows{"unknown": tc.rows}, queryErr: map[string]error{"unknown": tc.query}})
			if tc.rows != nil {
				tc.rows.i = 0
			}
			if err := db.Run(t.Context(), func(ctx context.Context) error {
				_, e := repo.ReadAnalytics(ctx, "u", domain.PeriodMonth, nil, domain.TransferWith)
				return e
			}); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestUnitDomainSnapshotOptionalColumnGroups(t *testing.T) {
	s := unitSnapshot("2026-09", domain.TransferWith)
	h, err := headerOf(s)
	if err != nil {
		t.Fatal(err)
	}
	key := keyOf(s.Key)
	for _, m := range []domain.Metric{{ID: "empty", Status: 4}, {ID: "basis", Status: 1, Basis: &domain.Basis{FullMonths: 1}}, {ID: "lock", Status: 3, Lock: &domain.Lock{Reason: 99}}, {ID: "filter", Status: 1, Filter: &domain.Filter{From: instant(1), To: instant(20)}}} {
		r, e := metricOf(key, m)
		if e != nil {
			t.Fatal(e)
		}
		got, e := domainSnapshot(key, h, nil, nil, []metricRow{r})
		if e != nil || !reflect.DeepEqual(got.Metrics[0], m) {
			t.Fatalf("mapping %+v %v", got, e)
		}
	}
	if _, err = domainSnapshot(key, h, []time.Time{*instant(1)}, nil, nil); !errors.Is(err, errBrokenHeader) {
		t.Fatal(err)
	}
	if enumInt(nil) != 0 {
		t.Fatal("null enum")
	}
}
