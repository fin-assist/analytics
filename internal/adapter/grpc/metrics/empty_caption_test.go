package metrics

import (
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitEmptyPeriodTextKeepsPrecisionNotesAfterNegation(t *testing.T) {
	for _, tc := range []struct {
		name, zone, tail string
		recalculating    bool
		coverage         domain.Coverage
	}{
		{"precise", "Europe/Moscow", "1–25 сентября нет", false, domain.CoveragePartial},
		{"rebuilding-partial", "UTC", "сентябрь 2026 нет · неполные данные · идёт пересчёт", true, domain.CoveragePartial},
		{"rebuilding-complete", "UTC", "сентябрь 2026 нет · идёт пересчёт", true, domain.CoverageComplete},
		{"retained-partial", "UTC", "сентябрь 2026 нет · неполные данные · сохранённый расчёт", false, domain.CoveragePartial},
		{"retained-complete", "UTC", "сентябрь 2026 нет · сохранённый расчёт", false, domain.CoverageComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := screenFixture(t)
			s.State.TimeZone = tc.zone
			s.State.Recalculating = tc.recalculating
			s.Snapshot.Coverage = tc.coverage
			for _, id := range []string{"expense_categories", "income_categories", "insights.bank_fees"} {
				m := fixtureMetric(&s, id)
				m.Status = 4
				m.ValueMinor = nil
			}
			r, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []struct{ id, prefix string }{{"expense_categories", "Расходы по категориям: за "}, {"income_categories", "Доходы по категориям: за "}, {"bank_fees", "Комиссий за "}} {
				b := byID(r, want.id)
				text := want.prefix + tc.tail
				if b.Status != "none" || b.GetEmptyText() != text {
					t.Fatalf("%s: %q, want %q", want.id, b.GetEmptyText(), text)
				}
				if want.id == "bank_fees" && b.GetAltText() != text {
					t.Fatalf("bank-fee alt_text: %q, want %q", b.GetAltText(), text)
				}
			}
		})
	}
}
