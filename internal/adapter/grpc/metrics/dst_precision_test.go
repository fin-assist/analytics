package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitRetainedDSTSnapshotDoesNotGuessInclusiveCoverageDay(t *testing.T) {
	for _, tc := range []struct {
		month, key, start, end, to, normal string
		typ                                domain.PeriodType
		coverage                           domain.Coverage
	}{
		{"2026-03", "2026-03", "2026-03-01T00:00:00+01:00", "2026-04-01T00:00:00+02:00", "2026-03-30T00:30:00+02:00", "1–30 марта", domain.PeriodMonth, domain.CoveragePartial},
		{"2026-10", "2026-10", "2026-10-01T00:00:00+02:00", "2026-11-01T00:00:00+01:00", "2026-10-28T23:59:59+01:00", "1–28 октября", domain.PeriodMonth, domain.CoveragePartial},
		{"2026-10", "2026-10", "2026-10-01T00:00:00+02:00", "2026-11-01T00:00:00+01:00", "2026-10-31T23:59:59+01:00", "1–31 октября", domain.PeriodMonth, domain.CoveragePartial},
		{"2026-12", "2026-Q4", "2026-10-01T00:00:00+02:00", "2027-01-01T00:00:00+01:00", "2026-12-31T23:59:59+01:00", "1 октября — 31 декабря", domain.PeriodQuarter, domain.CoverageComplete},
	} {
		t.Run(tc.key+tc.to, func(t *testing.T) {
			s := calendarFixture(t, tc.month, "Europe/Berlin", "Europe/Berlin")
			s.State.Recalculating = false
			start, _ := time.Parse(time.RFC3339, tc.start)
			end, _ := time.Parse(time.RFC3339, tc.end)
			to, _ := time.Parse(time.RFC3339, tc.to)
			s.Key.PeriodKey = domain.PeriodKey{Type: tc.typ, Key: tc.key}
			s.Range = domain.TimeRange{Start: &start, End: &end}
			s.Snapshot.Key = s.Key
			s.Snapshot.Range = &s.Range
			s.Snapshot.DataFrom = &start
			s.Snapshot.DataTo = &to
			s.Snapshot.Coverage = tc.coverage
			chart, err := metric(*fixtureMetric(&s, "monthly_chart"))
			if err != nil {
				t.Fatal(err)
			}
			last := chart.GetMonthlyChart().Points[len(chart.GetMonthlyChart().Points)-1]
			last.Bounds.DataTo = timestamppb.New(to)
			setDetails(t, &s, "monthly_chart", &analyticsv1.Metric{Details: chart.Details})
			c := composerForTest(t)
			normal, err := c.Compose(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if label := valueOf(byID(normal, "expense_categories"))["total"].(map[string]any)["label"]; label != "Всего · "+tc.normal {
				t.Fatalf("current IANA label %q", label)
			}
			if tc.key == "2026-03" || tc.to == "2026-10-28T23:59:59+01:00" {
				points := valueOf(byID(normal, "monthly_chart"))["points"].([]any)
				want := "по 30-е"
				if tc.key == "2026-10" {
					want = "по 28-е"
				}
				if note := points[len(points)-1].(map[string]any)["note"]; note != want {
					t.Fatalf("current IANA point %v, want %s", note, want)
				}
			}
			s.State.TimeZone = "UTC"
			s.State.Recalculating = true
			before, _ := json.Marshal(s.Snapshot)
			retained, err := c.Compose(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			label := valueOf(byID(retained, "expense_categories"))["total"].(map[string]any)["label"].(string)
			if !strings.Contains(label, "идёт пересчёт") || strings.Contains(label, "2027") {
				t.Fatalf("unrecoverable source day was asserted: %s", label)
			}
			if tc.coverage == domain.CoveragePartial && !strings.Contains(label, "неполные данные") {
				t.Fatal("partial data presented as the whole period")
			}
			point := valueOf(byID(retained, "monthly_chart"))["points"].([]any)
			note := point[len(point)-1].(map[string]any)["note"]
			if note != nil && note != "неполные данные" {
				t.Fatalf("guessed precise bar note: %v", note)
			}
			if !retained.Period.Range.Start.AsTime().Equal(start) || !retained.Period.Range.End.AsTime().Equal(end) || !retained.Period.Bounds.DataTo.AsTime().Equal(to) {
				t.Fatal("financial timestamps changed")
			}
			after, _ := json.Marshal(s.Snapshot)
			if string(before) != string(after) {
				t.Fatal("stored snapshot changed")
			}
		})
	}
}

func TestUnitRebuildingDoesNotInferIANAFromEqualEndpointOffsets(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	start, end := time.Date(2026, 1, 1, 0, 0, 0, 0, london), time.Date(2027, 1, 1, 0, 0, 0, 0, london)
	if !start.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("test endpoint calendars should be indistinguishable")
	}
	to := time.Date(2026, 6, 30, 0, 30, 0, 0, london)
	if to.Day() == to.In(time.UTC).Day() {
		t.Fatal("test operation days should differ")
	}
	s := calendarFixture(t, "2026-06", "Europe/London", "UTC")
	s.Key.PeriodKey = domain.PeriodKey{Type: domain.PeriodYear, Key: "2026"}
	s.Range = domain.TimeRange{Start: &start, End: &end}
	s.Snapshot.Range = &s.Range
	s.Snapshot.Key = s.Key
	s.Snapshot.DataTo = &to
	r, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	label := valueOf(byID(r, "expense_categories"))["total"].(map[string]any)["label"].(string)
	if !strings.Contains(label, "2026 год") || !strings.Contains(label, "неполные данные") || strings.Contains(label, "29 июня") {
		t.Fatal(label)
	}
}

func TestUnitUnknownCalendarKeepsAbsoluteAvailableFrom(t *testing.T) {
	s := screenFixture(t)
	s.State.Recalculating = true
	available := *s.Range.Start
	lock := &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH, AvailableFrom: timestamp(&available)}
	p := composition{s: s, zone: time.UTC}
	got := p.lock("forecast", lock)
	if strings.Contains(got.Text, "с 1") || !got.AvailableFrom.AsTime().Equal(available) {
		t.Fatal("guessed source date or altered condition")
	}
}

func TestUnitSettledMismatchedCalendarStillAvoidsGuessing(t *testing.T) {
	s := calendarFixture(t, "2026-03", "Europe/Berlin", "UTC")
	s.State.Recalculating = false
	r, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	label := valueOf(byID(r, "expense_categories"))["total"].(map[string]any)["label"]
	if label != "Всего · март 2026 · неполные данные · сохранённый расчёт" {
		t.Fatalf("mismatched source calendar: %v", label)
	}
}
