package domain

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestUnitRevisionMetadataIsOpenBoundedAndExact(t *testing.T) {
	valid := Revisions{"categories": 0, "future_source": math.MaxInt64, strings.Repeat("a", 64): 0}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []Revisions{{"": 0}, {"Categories": 0}, {"1code": 0}, {"time-zone": 0}, {strings.Repeat("a", 65): 0}, {"uploads": -1}} {
		if err := v.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal(v, err)
		}
	}
	maximum := Revisions{}
	for i := range 32 {
		maximum[fmt.Sprintf("source_%d", i)] = int64(i)
	}
	if err := maximum.Validate(); err != nil {
		t.Fatal(err)
	}
	maximum["extra"] = 0
	if err := maximum.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	left := Revisions{"categories": 1, "uploads": 2, "future_source": math.MaxInt64}
	right := Revisions{"categories": 2, "uploads": 2, "future_source": math.MaxInt64}
	if got := left.Intersect(right); !reflect.DeepEqual(got, Revisions{"uploads": 2, "future_source": math.MaxInt64}) {
		t.Fatal(got)
	}
	if len(left.Intersect(nil)) != 0 || len((Revisions(nil)).Intersect(right)) != 0 {
		t.Fatal("unknown source invented revisions")
	}
	snapshot := validSnapshot(PeriodMonth, "2026-09", TransferWith)
	snapshot.Revisions = Revisions{"bad": -1}
	if err := snapshot.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	state := validState()
	state.Revisions = Revisions{"bad": -1}
	if err := state.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
