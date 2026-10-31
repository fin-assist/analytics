package domain

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func at(year int, month time.Month, day int) *time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return &t
}

func validSnapshot(typ PeriodType, key string, mode TransferMode) Snapshot {
	return Snapshot{
		Key:      SnapshotKey{PeriodKey: PeriodKey{Type: typ, Key: key}, Mode: mode},
		Range:    &TimeRange{Start: at(2026, 9, 1), End: at(2026, 10, 1)},
		Coverage: CoveragePartial, DataFrom: at(2026, 9, 1), DataTo: at(2026, 9, 25),
		Gaps:         []TimeRange{{Start: at(2026, 9, 3), End: at(2026, 9, 4)}},
		CalculatedAt: at(2026, 9, 26),
		Metrics: []Metric{
			{ID: "tiles.expense", DefinitionVersion: 1, Status: 1, ValueMinor: ptr(int64(10)),
				Range:  &TimeRange{Start: at(2026, 8, 1), End: at(2026, 8, 26)},
				Lock:   &Lock{Reason: 2},
				Basis:  &Basis{Range: &TimeRange{Start: at(2026, 6, 1), End: at(2026, 9, 1)}, FullMonths: 3},
				Filter: &Filter{From: at(2026, 9, 1), To: at(2026, 10, 1), Kind: 1, TransferMode: 1}},
			{ID: "insights.future_metric", Status: 9, Basis: &Basis{FullMonths: 0}}, // unknown status of a newer producer
		},
	}
}

func validState() *State {
	return &State{
		HasData: true, DataFrom: at(2026, 9, 1), DataTo: at(2026, 9, 26), LastOperationAt: at(2026, 9, 25),
		StaleAfter: at(2026, 10, 10), FullMonths: 1, TimeZone: "UTC", CalculatedAt: at(2026, 9, 26),
	}
}

func validUpsert() Upsert {
	return Upsert{
		UserID: "01a107db-ae80-7b07-b8a1-000000000001", Version: 7, State: validState(),
		Snapshots: []Snapshot{
			validSnapshot(PeriodMonth, "2026-09", TransferWith),
			validSnapshot(PeriodMonth, "2026-09", TransferWithout),
			validSnapshot(PeriodQuarter, "2026-Q3", TransferWith),
			validSnapshot(PeriodYear, "2026", TransferWithout),
		},
		Removed: []PeriodKey{{Type: PeriodMonth, Key: "2026-07"}, {Type: PeriodQuarter, Key: "2026-Q2"}},
	}
}

func TestUnitValidUpsert(t *testing.T) {
	if err := validUpsert().Validate(); err != nil {
		t.Fatal(err)
	}
	// Neither state nor periods: a valid, empty batch. Validation accepts a UUID of either case;
	// the handler canonicalizes it to lower case before the usecase (canonicalUserID), so the
	// tombstone lock and comparisons see one form.
	for _, id := range []string{"01A107DB-AE80-7B07-B8A1-000000000001", "01a107db-ae80-7b07-b8a1-000000000001"} {
		if err := (Upsert{UserID: id, Version: 1}).Validate(); err != nil {
			t.Fatalf("empty batch of %s: %v", id, err)
		}
	}
	// A state without bounds: a user without covered days.
	u := validUpsert()
	u.State = &State{TimeZone: "UTC", CalculatedAt: at(2026, 9, 26)}
	if err := u.Validate(); err != nil {
		t.Fatalf("state without data: %v", err)
	}
	u.State = validState()
	u.State.HasData, u.State.LastOperationAt = false, nil
	if err := u.Validate(); err != nil {
		t.Fatalf("covered days without operations: %v", err)
	}
	u.Version = math.MaxInt32
	if err := u.Validate(); err != nil {
		t.Fatalf("largest version: %v", err)
	}
}

// Every rule of «Инварианты в коде сервиса» refuses a batch that breaks it, wrapped in
// ErrInvalid, and names no user data.
func TestUnitUpsertRefusesEveryBrokenRule(t *testing.T) {
	first := func(u *Upsert) *Snapshot { return &u.Snapshots[0] }
	metric := func(u *Upsert) *Metric { return &u.Snapshots[0].Metrics[0] }
	cases := map[string]func(u *Upsert){
		"user_id empty":            func(u *Upsert) { u.UserID = "" },
		"user_id not a UUID":       func(u *Upsert) { u.UserID = "user-1" },
		"user_id with braces":      func(u *Upsert) { u.UserID = "{01a107db-ae80-7b07-b8a1-000000000001}" },
		"calc_version zero":        func(u *Upsert) { u.Version = 0 },
		"calc_version negative":    func(u *Upsert) { u.Version = -1 },
		"period type unspecified":  func(u *Upsert) { first(u).Key.Type = 0 },
		"period type unknown":      func(u *Upsert) { first(u).Key.Type = 99 },
		"transfer mode unset":      func(u *Upsert) { first(u).Key.Mode = 0 },
		"transfer mode unknown":    func(u *Upsert) { first(u).Key.Mode = 3 },
		"month 2026-9":             func(u *Upsert) { first(u).Key.Key = "2026-9" },
		"month 2026-13":            func(u *Upsert) { first(u).Key.Key = "2026-13" },
		"month 2026-00":            func(u *Upsert) { first(u).Key.Key = "2026-00" },
		"month with a space":       func(u *Upsert) { first(u).Key.Key = "2026-09 " },
		"month as a quarter":       func(u *Upsert) { first(u).Key.Key = "2026-Q3" },
		"quarter lower case":       func(u *Upsert) { u.Snapshots[2].Key.Key = "2026-q3" },
		"quarter 5":                func(u *Upsert) { u.Snapshots[2].Key.Key = "2026-Q5" },
		"year 02026":               func(u *Upsert) { u.Snapshots[3].Key.Key = "02026" },
		"same snapshot twice":      func(u *Upsert) { u.Snapshots = append(u.Snapshots, u.Snapshots[0]) },
		"removed twice":            func(u *Upsert) { u.Removed = append(u.Removed, u.Removed[0]) },
		"sent and removed":         func(u *Upsert) { u.Removed = append(u.Removed, PeriodKey{Type: PeriodMonth, Key: "2026-09"}) },
		"removed with a bad key":   func(u *Upsert) { u.Removed[0].Key = "2026-7" },
		"removed with a bad type":  func(u *Upsert) { u.Removed[0].Type = 0 },
		"no range":                 func(u *Upsert) { first(u).Range = nil },
		"range without end":        func(u *Upsert) { first(u).Range.End = nil },
		"range reversed":           func(u *Upsert) { first(u).Range.Start = at(2026, 10, 2) },
		"range empty":              func(u *Upsert) { first(u).Range.End = first(u).Range.Start },
		"coverage unset":           func(u *Upsert) { first(u).Coverage = 0 },
		"coverage no data":         func(u *Upsert) { first(u).Coverage = 3 },
		"coverage unknown":         func(u *Upsert) { first(u).Coverage = 70000 },
		"no data_from":             func(u *Upsert) { first(u).DataFrom = nil },
		"no data_to":               func(u *Upsert) { first(u).DataTo = nil },
		"no calculated_at":         func(u *Upsert) { first(u).CalculatedAt = nil },
		"gap without start":        func(u *Upsert) { first(u).Gaps[0].Start = nil },
		"gap reversed":             func(u *Upsert) { first(u).Gaps[0].End = at(2026, 9, 2) },
		"too many gaps":            func(u *Upsert) { first(u).Gaps = make([]TimeRange, maxGaps+1) },
		"metric without id":        func(u *Upsert) { metric(u).ID = "" },
		"metric twice":             func(u *Upsert) { first(u).Metrics[1].ID = metric(u).ID },
		"status unspecified":       func(u *Upsert) { metric(u).Status = 0 },
		"status unavailable":       func(u *Upsert) { metric(u).Status = StatusUnavailable },
		"status beyond smallint":   func(u *Upsert) { metric(u).Status = 65537 },
		"metric range half":        func(u *Upsert) { metric(u).Range.Start = nil },
		"lock without reason":      func(u *Upsert) { metric(u).Lock.Reason = 0 },
		"lock reason beyond":       func(u *Upsert) { metric(u).Lock.Reason = -40000 },
		"basis range reversed":     func(u *Upsert) { metric(u).Basis.Range.End = at(2026, 5, 1) },
		"filter without from":      func(u *Upsert) { metric(u).Filter.From = nil },
		"filter without to":        func(u *Upsert) { metric(u).Filter.To = nil },
		"filter kind beyond":       func(u *Upsert) { metric(u).Filter.Kind = 32768 },
		"filter mode beyond":       func(u *Upsert) { metric(u).Filter.TransferMode = -32769 },
		"state no calculated_at":   func(u *Upsert) { u.State.CalculatedAt = nil },
		"state one bound":          func(u *Upsert) { u.State.DataTo = nil },
		"state bounds reversed":    func(u *Upsert) { u.State.DataTo = u.State.DataFrom },
		"state stale without data": func(u *Upsert) { u.State.DataFrom, u.State.DataTo, u.State.HasData = nil, nil, false },
		"state data without stale": func(u *Upsert) { u.State.StaleAfter = nil },
		"state stale not after":    func(u *Upsert) { u.State.StaleAfter = u.State.DataTo },
		"state data without ops":   func(u *Upsert) { u.State.LastOperationAt = nil },
		"state full_months":        func(u *Upsert) { u.State.FullMonths = -1 },
		"state time zone":          func(u *Upsert) { u.State.TimeZone = "Mars/Olympus" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			u := validUpsert()
			change(&u)
			err := u.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v, want ErrInvalid", err)
			}
			if strings.Contains(err.Error(), u.UserID) && u.UserID != "" {
				t.Fatalf("error names the user: %v", err)
			}
		})
	}
}

func TestUnitValidateTimeZone(t *testing.T) {
	for _, ok := range []string{"Europe/Moscow", "UTC", "Asia/Yekaterinburg", "America/Argentina/Buenos_Aires"} {
		if err := ValidateTimeZone(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	long := strings.Repeat("A", maxTimeZone+1)
	for _, bad := range []string{"", "Local", "./Etc/UTC", "Etc//UTC", "../etc/passwd", "Europe/../UTC", "/UTC", "UTC/", long, "Not/AZone", "Europe/Moscow\x00"} {
		if err := ValidateTimeZone(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestUnitIncompletePeriods(t *testing.T) {
	u := validUpsert()
	got := u.IncompletePeriods()
	want := []PeriodKey{{PeriodQuarter, "2026-Q3"}, {PeriodYear, "2026"}}
	if !slices.Equal(got, want) {
		t.Fatalf("incomplete %v, want %v", got, want)
	}
	if got := (Upsert{}).IncompletePeriods(); got != nil {
		t.Fatalf("empty batch: %v", got)
	}
}

func TestUnitPeriodType(t *testing.T) {
	for typ, name := range map[PeriodType]string{PeriodMonth: "month", PeriodQuarter: "quarter", PeriodYear: "year", PeriodAllTime: "all_time", 0: "unknown", 9: "unknown"} {
		if typ.String() != name {
			t.Errorf("%d is %q, want %q", typ, typ.String(), name)
		}
		if typ.Known() != (name != "unknown") {
			t.Errorf("%d Known %v", typ, typ.Known())
		}
	}
}

func TestUnitValidUserID(t *testing.T) {
	if !ValidUserID("01a107db-ae80-7b07-b8a1-000000000001") || ValidUserID("01a107dbae807b07b8a1000000000001") || ValidUserID("u1") {
		t.Fatal("ValidUserID")
	}
}

func ptr[T any](v T) *T { return &v }

// Hostile keys and zone names of a producer end in ErrInvalid, never a panic; every accepted
// key has the fixed length of its grammar, so the byte order of keys is the calendar order.
func FuzzUnitPeriodKeyAndTimeZone(f *testing.F) {
	for _, seed := range []string{"2026-09", "2026-Q3", "2026", "2026-9", "../UTC", "Europe/Moscow", "", "Local"} {
		f.Add(int32(1), seed)
	}
	f.Add(int32(4), "all_time")
	f.Fuzz(func(t *testing.T, typ int32, key string) {
		err := PeriodKey{Type: PeriodType(typ), Key: key}.Validate()
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("error %v is not ErrInvalid", err)
		}
		if err == nil {
			want := map[PeriodType]int{PeriodMonth: 7, PeriodQuarter: 7, PeriodYear: 4, PeriodAllTime: 8}[PeriodType(typ)]
			if len(key) != want {
				t.Fatalf("accepted %q of type %d", key, typ)
			}
		}
		if err := ValidateTimeZone(key); err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("zone error %v is not ErrInvalid", err)
		}
	})
}
