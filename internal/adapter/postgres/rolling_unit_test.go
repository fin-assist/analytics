package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

func rollingListRow() []any {
	from := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	version := int32(4)
	coverage := int16(domain.CoverageComplete)
	revisions := []byte(`{"calc_version":4,"revisions":{"uploads":1}}`)
	return []any{"UTC", &from, &end, revisions, &version, &from, &end, &coverage, revisions, &version}
}

func TestUnitRollingListUsesCoherentHeaderAndRejectsBrokenMetadata(t *testing.T) {
	boom := errors.New("read failed")
	for _, tc := range []struct {
		name                   string
		row                    []any
		scan, iteration, query error
		want                   error
		coverage               domain.Coverage
		items                  int
	}{
		{name: "complete", row: rollingListRow(), coverage: domain.CoverageComplete, items: 1},
		{name: "missing header", row: func() []any {
			r := rollingListRow()
			for i := 5; i < len(r); i++ {
				r[i] = nil
			}
			return r
		}(), coverage: domain.CoveragePartial, items: 1},
		{name: "old anchor", row: func() []any { r := rollingListRow(); r[5] = instant(1); return r }(), coverage: domain.CoveragePartial, items: 1},
		{name: "no coverage", row: func() []any { r := rollingListRow(); r[1] = nil; r[2] = nil; return r }()},
		{name: "no state"},
		{name: "query", query: boom, want: boom},
		{name: "scan", row: rollingListRow(), scan: boom, want: boom},
		{name: "iteration", iteration: boom, want: boom},
		{name: "broken header", row: func() []any { r := rollingListRow(); r[7] = nil; return r }(), want: errBrokenHeader},
		{name: "state revisions", row: func() []any {
			r := rollingListRow()
			r[3] = []byte(`{"calc_version":4,"revisions":{"uploads":null}}`)
			return r
		}(), want: errStoredRevisions},
		{name: "header revisions", row: func() []any {
			r := rollingListRow()
			r[8] = []byte(`{"calc_version":4,"revisions":{"uploads":null}}`)
			return r
		}(), want: errStoredRevisions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &scriptedRows{scanErr: tc.scan, err: tc.iteration}
			if tc.row != nil {
				rows.values = [][]any{tc.row}
			}
			tx := &scriptedTx{rows: map[string]*scriptedRows{"unknown": rows}, queryErr: map[string]error{"unknown": tc.query}}
			repo, db := unitRepository(tx)
			var got domain.Periods
			err := db.Run(t.Context(), func(ctx context.Context) error {
				var e error
				got, e = repo.ListPeriods(ctx, "user", domain.PeriodRollingQuarter)
				return e
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("%v want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if len(got.Items) != tc.items {
				t.Fatal(got)
			}
			if tc.items > 0 && got.Items[0].Coverage != tc.coverage {
				t.Fatal(got)
			}
			if tc.name == "complete" && !reflect.DeepEqual(got.Revisions, domain.Revisions{"uploads": 1}) {
				t.Fatal(got.Revisions)
			}
		})
	}
}

func TestUnitRollingListCollectsBothHeadersBeforeDomainSelection(t *testing.T) {
	stale, current := rollingListRow(), rollingListRow()
	stale[5] = instant(1)
	tx := &scriptedTx{rows: map[string]*scriptedRows{"unknown": {values: [][]any{stale, current}}}}
	repo, db := unitRepository(tx)
	var got domain.Periods
	err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		got, err = repo.ListPeriods(ctx, "user", domain.PeriodRollingQuarter)
		return err
	})
	if err != nil || len(got.Items) != 1 || got.Items[0].Coverage != domain.CoverageComplete || len(got.Revisions) != 1 || got.Revisions["uploads"] != 1 {
		t.Fatalf("second coherent header lost: %+v %v", got, err)
	}
}
