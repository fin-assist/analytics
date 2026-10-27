package postgres

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitRevisionJSONPreservesInt64AndRejectsCorruptStorage(t *testing.T) {
	values := domain.Revisions{"uploads": math.MaxInt64, "future_source": 0}
	data := revisionsJSON(values)
	if string(data) != `{"future_source":0,"uploads":9223372036854775807}` {
		t.Fatal(string(data))
	}
	got, err := revisionsFromJSON(storedRevisions(string(data)), ptr(int32(1)))
	if err != nil || got["uploads"] != math.MaxInt64 || got["future_source"] != 0 {
		t.Fatal(got, err)
	}
	if revisionsJSON(nil) != nil {
		t.Fatal("unknown source synthesized data")
	}
	for _, empty := range [][]byte{nil, []byte(`null`), []byte(`{}`)} {
		if got, err := revisionsFromJSON(empty, ptr(int32(1))); err != nil || len(got) != 0 {
			t.Fatal(got, err)
		}
	}
	for _, bad := range []string{`{`, `[]`, `{"uploads":-1}`, `{"uploads":9223372036854775808}`, `{"Upper":0}`, `{"uploads":null}`, `{"uploads":0.0}`, `{"uploads":true}`, `{"uploads":"0"}`, `{"uploads":{}}`, `{"uploads":[]}`} {
		if _, err := revisionsFromJSON(storedRevisions(bad), ptr(int32(1))); !errors.Is(err, errStoredRevisions) {
			t.Fatal(bad, err)
		}
	}
}
func TestUnitAnalyticsReadRejectsCorruptStoredProvenance(t *testing.T) {
	for _, index := range []int{22, 23} {
		row := analyticsRow()
		row[index] = storedRevisions(`{"uploads":-1}`)
		repo, db := unitRepository(&scriptedTx{rows: map[string]*scriptedRows{"unknown": {values: [][]any{row}}}})
		if err := db.Run(t.Context(), func(c context.Context) error {
			_, e := repo.ReadAnalytics(c, "u", domain.PeriodMonth, nil, domain.TransferWith)
			return e
		}); !errors.Is(err, errStoredRevisions) {
			t.Fatal(index, err)
		}
	}
}

func TestUnitPeriodProvenanceIntersectionDoesNotRestartAfterUnknownHeader(t *testing.T) {
	zone, key1, key2 := "Europe/Moscow", "2026-10", "2026-09"
	start, end := *instant(1), *instant(30)
	coverage := int16(1)
	tx := &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{
		{&zone, &key1, &start, &end, &coverage, storedRevisions(`{"uploads":9}`), nil, ptr(int32(1)), ptr(int32(1))},
		{&zone, &key2, &start, &end, &coverage, storedRevisions(`{"uploads":9}`), storedRevisions(`{"uploads":8,"future_source":0}`), ptr(int32(1)), ptr(int32(1))},
	}}}}
	got, err := listPeriods(t.Context(), tx, "u", domain.PeriodMonth)
	if err != nil || len(got.Items) != 2 || len(got.Revisions) != 0 {
		t.Fatal("later known source overwrote unknown baseline", got, err)
	}
	for _, column := range []int{5, 6} {
		row := []any{&zone, &key1, &start, &end, &coverage, nil, nil, ptr(int32(1)), ptr(int32(1))}
		row[column] = storedRevisions(`{"uploads":-1}`)
		tx = &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{row}}}}
		if _, err = listPeriods(t.Context(), tx, "u", domain.PeriodMonth); !errors.Is(err, errStoredRevisions) {
			t.Fatal("corrupt stored list provenance accepted", column, err)
		}
	}
}

func TestUnitPeriodProvenanceIncludesUsedScreenState(t *testing.T) {
	zone, key := "Europe/Moscow", "2026-09"
	start, end, coverage := *instant(1), *instant(30), int16(1)
	for _, state := range [][]byte{nil, storedRevisions(`{"uploads":4,"categories":1}`)} {
		tx := &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{{&zone, &key, &start, &end, &coverage, state, storedRevisions(`{"uploads":5,"categories":1}`), ptr(int32(1)), ptr(int32(1))}}}}}
		got, err := listPeriods(t.Context(), tx, "u", domain.PeriodMonth)
		expected := 0
		if state != nil {
			expected = 1
		}
		if err != nil || len(got.Revisions) != expected {
			t.Fatal(got, err)
		}
		if state != nil && got.Revisions["categories"] != 1 {
			t.Fatal(got)
		}
	}
}

// These are storage fixtures, distinct from the raw map passed to the write SQL.
func storedRevisions(raw string) []byte {
	return []byte(`{"calc_version":1,"revisions":` + raw + `}`)
}

func TestUnitStoredProvenanceIsBoundToItsFinancialRowVersion(t *testing.T) {
	payload := storedRevisions(`{"uploads":9223372036854775807,"future_source":0}`)
	for _, version := range []*int32{nil, ptr(int32(0)), ptr(int32(2))} {
		got, err := revisionsFromJSON(payload, version)
		if err != nil || len(got) != 0 {
			t.Fatal("mismatched row version certified stale provenance", version, got, err)
		}
	}
	got, err := revisionsFromJSON(payload, ptr(int32(1)))
	if err != nil || len(got) != 2 || got["uploads"] != math.MaxInt64 {
		t.Fatal("matching version lost exact provenance", got, err)
	}
	for _, bad := range []string{
		`{"calc_version":null,"revisions":{"uploads":0}}`,
		`{"calc_version":1.0,"revisions":{"uploads":0}}`,
		`{"calc_version":2147483648,"revisions":{"uploads":0}}`,
		`{"calc_version":-1,"revisions":{"uploads":0}}`,
		`{"revisions":{"uploads":0}}`,
		`{"calc_version":1,"revisions":null}`,
		`{"calc_version":1}`,
	} {
		if _, err := revisionsFromJSON([]byte(bad), ptr(int32(1))); !errors.Is(err, errStoredRevisions) {
			t.Fatal("invalid storage envelope accepted", bad, err)
		}
	}
}
