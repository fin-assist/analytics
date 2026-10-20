package migrations_test

// Unit tests of the PeriodSnapshot ↔ rows mapping (snapshot_rows_test.go) without PostgreSQL:
// the enum range checks at their exact bounds, the refusals of each mapper and the checks of a
// header on read. The round trip through the tables is in schema_test.go.

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestSmallintOf(t *testing.T) {
	cases := []struct {
		in   int32
		want int16
		err  bool
	}{
		{math.MinInt16 - 1, 0, true},
		{math.MinInt16, math.MinInt16, false},
		{0, 0, false},
		{9, 9, false}, // an unknown number of a newer producer is kept
		{math.MaxInt16, math.MaxInt16, false},
		{math.MaxInt16 + 1, 0, true},
		{65537, 0, true}, // would truncate to 1 (READY)
		{math.MaxInt32, 0, true},
	}
	for _, c := range cases {
		got, err := smallintOf(analyticsv1.MetricStatus(c.in))
		if c.err != errors.Is(err, errEnumRange) || got != c.want {
			t.Errorf("smallintOf(%d) = %d, %v; want %d, error %v", c.in, got, err, c.want, c.err)
		}
	}
}

// calc_version is integer in the schema and int64 in the contract: the bounds of int32 map, one
// beyond is refused.
func TestCalcVersionOf(t *testing.T) {
	for _, c := range []struct {
		in  int64
		err bool
	}{
		{math.MinInt32 - 1, true}, {math.MinInt32, false}, {1, false}, {math.MaxInt32, false},
		{math.MaxInt32 + 1, true}, {math.MaxInt64, true},
	} {
		got, err := calcVersionOf(c.in)
		if c.err != errors.Is(err, errVersionRange) || (!c.err && int64(got) != c.in) {
			t.Errorf("calcVersionOf(%d) = %d, %v; want error %v", c.in, got, err, c.err)
		}
	}
}

func TestEnumColumn(t *testing.T) {
	if got, err := enumColumn(commonv1.KindFilter_KIND_FILTER_UNSPECIFIED); got != nil || err != nil {
		t.Fatalf("UNSPECIFIED: %v, %v; want NULL", got, err)
	}
	if got, err := enumColumn(commonv1.KindFilter_KIND_FILTER_INCOME); err != nil || got == nil || *got != 2 {
		t.Fatalf("INCOME: %v, %v; want 2", got, err)
	}
	if got, err := enumColumn(commonv1.KindFilter(math.MaxInt16)); err != nil || got == nil || *got != math.MaxInt16 {
		t.Fatalf("MaxInt16: %v, %v", got, err)
	}
	if _, err := enumColumn(commonv1.KindFilter(math.MaxInt16 + 1)); !errors.Is(err, errEnumRange) {
		t.Fatalf("MaxInt16+1: %v, want %v", err, errEnumRange)
	}
}

// metricOf refuses every enum column out of range and names the metric; a valid metric maps.
func TestMetricOfRefusesEnumsOutOfRange(t *testing.T) {
	start, end := periodRange(month, "2026-09")
	filter := func() *commonv1.OperationsFilter {
		return &commonv1.OperationsFilter{From: at(start), To: at(end)}
	}
	cases := map[string]func(m *analyticsv1.Metric){
		"status": func(m *analyticsv1.Metric) { m.Status = analyticsv1.MetricStatus(math.MaxInt16 + 1) },
		"lock reason": func(m *analyticsv1.Metric) {
			m.Lock = &analyticsv1.MetricLock{Reason: analyticsv1.LockReason(math.MinInt16 - 1)}
		},
		"filter kind": func(m *analyticsv1.Metric) { m.Filters = filter(); m.Filters.Kind = commonv1.KindFilter(70000) },
		"filter transfer mode": func(m *analyticsv1.Metric) {
			m.Filters = filter()
			m.Filters.TransferMode = commonv1.TransferMode(70000)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_READY}
			change(m)
			_, err := metricOf(pk(month, "2026-09", withTransfers), &analyticsv1.MetricSnapshot{MetricId: "tiles.expense", Metric: m})
			if !errors.Is(err, errEnumRange) {
				t.Fatalf("error %v, want %v", err, errEnumRange)
			}
		})
	}

	m := &analyticsv1.Metric{
		Status:  analyticsv1.MetricStatus(math.MaxInt16),
		Lock:    &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS, Text: "не хранится"},
		Filters: &commonv1.OperationsFilter{From: at(start), To: at(end), TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT},
	}
	r, err := metricOf(pk(month, "2026-09", withTransfers), &analyticsv1.MetricSnapshot{MetricId: "tiles.expense", Metric: m})
	if err != nil {
		t.Fatal(err)
	}
	if r.status != math.MaxInt16 || *r.lockReason != 1 || r.filterKind != nil || *r.filterTransferMode != withoutTransfers {
		t.Fatalf("row %+v", r)
	}
}

func TestHeaderOfRefusesCoverageOutOfRange(t *testing.T) {
	s := snapshot(month, "2026-09", withTransfers, 1)
	s.Bounds.Coverage = commonv1.Coverage(math.MaxInt16 + 1)
	if _, err := headerOf(s); !errors.Is(err, errEnumRange) {
		t.Fatalf("error %v, want %v", err, errEnumRange)
	}
	s.Bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
	h, err := headerOf(s)
	if err != nil || *h.coverage != complete || h.removed {
		t.Fatalf("header %+v, %v", h, err)
	}
}

// A snapshot header without its data is refused on write as on read.
func TestHeaderOfRefusesSnapshotWithoutData(t *testing.T) {
	cases := map[string]func(s *analyticsv1.PeriodSnapshot){
		"no range":         func(s *analyticsv1.PeriodSnapshot) { s.Range = nil },
		"no range end":     func(s *analyticsv1.PeriodSnapshot) { s.Range.End = nil },
		"no bounds":        func(s *analyticsv1.PeriodSnapshot) { s.Bounds = nil },
		"coverage unset":   func(s *analyticsv1.PeriodSnapshot) { s.Bounds.Coverage = commonv1.Coverage_COVERAGE_UNSPECIFIED },
		"no calculated_at": func(s *analyticsv1.PeriodSnapshot) { s.CalculatedAt = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := snapshot(month, "2026-09", withTransfers, 1)
			change(s)
			if _, err := headerOf(s); !errors.Is(err, errBrokenHeader) {
				t.Fatalf("error %v, want %v", err, errBrokenHeader)
			}
		})
	}
}

// The mapping alone, without PostgreSQL: PeriodSnapshot → header, gap and metric rows →
// PeriodSnapshot is the same message, for every column and every kind of details. The same
// snapshot goes through the tables in TestSnapshotRoundTrip.
func TestMappingRoundTrip(t *testing.T) {
	for name, in := range map[string]*analyticsv1.PeriodSnapshot{
		"every column and every kind of details": richSnapshot(),
		"no metrics, gaps and data bounds":       snapshot(quarter, "2026-Q3", withoutTransfers, 0),
	} {
		t.Run(name, func(t *testing.T) {
			h, err := headerOf(in)
			if err != nil {
				t.Fatal(err)
			}
			gaps, err := gapsOf(in)
			if err != nil {
				t.Fatal(err)
			}
			var starts, ends []time.Time
			for _, g := range gaps {
				starts, ends = append(starts, *g.start), append(ends, *g.end)
			}
			var metrics []metricRow
			for _, ms := range in.GetMetrics() {
				r, err := metricOf(keyOf(in), ms)
				if err != nil {
					t.Fatal(err)
				}
				metrics = append(metrics, r)
			}
			// The read returns metrics in metric_id order (ORDER BY in readAnalyticsSQL).
			slices.SortFunc(metrics, func(a, b metricRow) int { return strings.Compare(a.metricID, b.metricID) })
			got, err := snapshotOf(keyOf(in), h, starts, ends, metrics)
			if err != nil {
				t.Fatal(err)
			}
			if want := sortedByMetricID(in); !proto.Equal(got, want) {
				t.Fatalf("snapshot came back as\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// The ordinal is smallint: 32768 gaps (ordinals 0…32767) map, one more is refused. A gap
// without a bound maps to NULL, which NOT NULL refuses, never to the epoch.
func TestGapsOf(t *testing.T) {
	s := snapshot(month, "2026-09", withTransfers, 1)
	day := timeRange(time.Date(2026, 9, 3, 0, 0, 0, 0, msk), time.Date(2026, 9, 4, 0, 0, 0, 0, msk))
	for range math.MaxInt16 + 1 {
		s.Bounds.Gaps = append(s.Bounds.Gaps, day)
	}
	gaps, err := gapsOf(s)
	if err != nil || len(gaps) != math.MaxInt16+1 || gaps[len(gaps)-1].ordinal != math.MaxInt16 {
		t.Fatalf("32768 gaps: %d, %v", len(gaps), err)
	}
	s.Bounds.Gaps = append(s.Bounds.Gaps, day)
	if _, err := gapsOf(s); err == nil {
		t.Fatal("32769 gaps mapped, want an error")
	}

	s.Bounds.Gaps = []*commonv1.TimeRange{{Start: day.GetStart()}}
	gaps, err = gapsOf(s)
	if err != nil || gaps[0].start == nil || gaps[0].end != nil {
		t.Fatalf("gap without its end: %+v, %v; want end NULL", gaps, err)
	}
}

// A header with removed = false must carry its data: any empty data column fails the read.
func TestSnapshotOfRefusesBrokenHeader(t *testing.T) {
	key := pk(month, "2026-09", withTransfers)
	valid := func() headerRow {
		h, err := headerOf(snapshot(month, "2026-09", withTransfers, 1))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if _, err := snapshotOf(key, valid(), nil, nil, nil); err != nil {
		t.Fatalf("valid header: %v", err)
	}
	cases := map[string]func(h *headerRow){
		"range_start":        func(h *headerRow) { h.rangeStart = nil },
		"range_end":          func(h *headerRow) { h.rangeEnd = nil },
		"coverage":           func(h *headerRow) { h.coverage = nil },
		"unread_lines_count": func(h *headerRow) { h.unreadLines = nil },
		"calculated_at":      func(h *headerRow) { h.calculatedAt = nil },
	}
	for column, change := range cases {
		t.Run(column, func(t *testing.T) {
			h := valid()
			change(&h)
			if _, err := snapshotOf(key, h, nil, nil, nil); !errors.Is(err, errBrokenHeader) {
				t.Fatalf("error %v, want %v", err, errBrokenHeader)
			}
		})
	}
	// data_from and data_to may be empty: a period without covered days in its bounds.
	h := valid()
	h.dataFrom, h.dataTo = nil, nil
	if s, err := snapshotOf(key, h, nil, nil, nil); err != nil || s.GetBounds().GetDataFrom() != nil {
		t.Fatalf("header without data bounds: %v, %v", s, err)
	}
}

// Details that are not protojson of a Metric fail the read with the metric named.
func TestSnapshotOfRefusesBrokenDetails(t *testing.T) {
	h, err := headerOf(snapshot(month, "2026-09", withTransfers, 1))
	if err != nil {
		t.Fatal(err)
	}
	broken := metricRow{metricID: "expense_categories", status: 1, details: []byte(`{"categories": 5}`)}
	if _, err := snapshotOf(pk(month, "2026-09", withTransfers), h, nil, nil, []metricRow{broken}); err == nil {
		t.Fatal("broken details read, want an error")
	}
	ok := metricRow{metricID: "insights.bank_fees", status: 4, details: []byte(`{"bank_fees": {}}`)}
	s, err := snapshotOf(pk(month, "2026-09", withTransfers), h, nil, nil, []metricRow{ok})
	if err != nil || s.GetMetrics()[0].GetMetric().GetBankFees() == nil ||
		!proto.Equal(s.GetMetrics()[0].GetMetric().GetBankFees(), &analyticsv1.BankFees{}) {
		t.Fatalf("empty bank_fees: %v, %v", s, err)
	}
}
