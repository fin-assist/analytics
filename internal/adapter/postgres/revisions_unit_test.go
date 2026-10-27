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
	got, err := revisionsFromJSON(data)
	if err != nil || got["uploads"] != math.MaxInt64 || got["future_source"] != 0 {
		t.Fatal(got, err)
	}
	if revisionsJSON(nil) != nil {
		t.Fatal("unknown source synthesized data")
	}
	for _, empty := range [][]byte{nil, []byte(`null`), []byte(`{}`)} {
		if got, err := revisionsFromJSON(empty); err != nil || len(got) != 0 {
			t.Fatal(got, err)
		}
	}
	for _, bad := range []string{`{`, `[]`, `{"uploads":-1}`, `{"uploads":9223372036854775808}`, `{"Upper":0}`, `{"uploads":null}`, `{"uploads":0.0}`, `{"uploads":true}`, `{"uploads":"0"}`, `{"uploads":{}}`, `{"uploads":[]}`} {
		if _, err := revisionsFromJSON([]byte(bad)); !errors.Is(err, errStoredRevisions) {
			t.Fatal(bad, err)
		}
	}
}
func TestUnitAnalyticsReadRejectsCorruptStoredProvenance(t *testing.T) {
	for _, index := range []int{22, 23} {
		row := analyticsRow()
		row[index] = []byte(`{"uploads":-1}`)
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
		{&zone, &key1, &start, &end, &coverage, []byte(`{"uploads":9}`), nil},
		{&zone, &key2, &start, &end, &coverage, []byte(`{"uploads":9}`), []byte(`{"uploads":8,"future_source":0}`)},
	}}}}
	got, err := listPeriods(t.Context(), tx, "u", domain.PeriodMonth)
	if err != nil || len(got.Items) != 2 || len(got.Revisions) != 0 {
		t.Fatal("later known source overwrote unknown baseline", got, err)
	}
	for _, column := range []int{5, 6} {
		row := []any{&zone, &key1, &start, &end, &coverage, nil, nil}
		row[column] = []byte(`{"uploads":-1}`)
		tx = &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{row}}}}
		if _, err = listPeriods(t.Context(), tx, "u", domain.PeriodMonth); !errors.Is(err, errStoredRevisions) {
			t.Fatal("corrupt stored list provenance accepted", column, err)
		}
	}
}

func TestUnitPeriodProvenanceIncludesUsedScreenState(t *testing.T) {
	zone, key := "Europe/Moscow", "2026-09"
	start, end, coverage := *instant(1), *instant(30), int16(1)
	for _, state := range [][]byte{nil, []byte(`{"uploads":4,"categories":1}`)} {
		tx := &scriptedTx{rows: map[string]*scriptedRows{"list": {values: [][]any{{&zone, &key, &start, &end, &coverage, state, []byte(`{"uploads":5,"categories":1}`)}}}}}
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
