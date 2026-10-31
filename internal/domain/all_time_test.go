package domain

import (
	"errors"
	"testing"
)

func TestUnitAllTimeIsAnExactNonCalendarStorageKey(t *testing.T) {
	if !PeriodAllTime.Known() || PeriodAllTime.String() != "all_time" {
		t.Fatal("all-time type is not stored")
	}
	for _, mode := range TransferModes {
		s := validSnapshot(PeriodAllTime, "all_time", mode)
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", "2026", "2026-10", "ALL_TIME", "all_time ", "../all_time"} {
		if err := (PeriodKey{PeriodAllTime, key}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted all_time key %q: %v", key, err)
		}
	}
	for _, typ := range []PeriodType{PeriodMonth, PeriodQuarter, PeriodYear, 5} {
		if err := (PeriodKey{typ, "all_time"}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted all_time as %v: %v", typ, err)
		}
	}
	u := validUpsert()
	u.Removed = append(u.Removed, PeriodKey{PeriodAllTime, "all_time"})
	if err := u.Validate(); err != nil {
		t.Fatal(err)
	}
}
