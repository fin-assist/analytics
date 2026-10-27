package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

func (p composition) chart(ctx context.Context, id string) (*commonv1.Block, error) {
	m, err := p.get("monthly_chart")
	if id == "balance" {
		if err != nil {
			m = &analyticsv1.Metric{}
		}
		return p.balanceChart(ctx, m)
	}
	if err != nil {
		return nil, err
	}
	b := p.base(id, m)
	if !ready(m) {
		return b, nil
	}
	chart := m.GetMonthlyChart()
	if chart == nil {
		return nil, errors.New("monthly chart details absent")
	}
	points := []any{}
	alt := []string{}
	for _, point := range chart.Points {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if point.GetRange().GetStart() == nil || point.GetRange().GetEnd() == nil {
			return nil, errors.New("monthly point range absent")
		}
		var value any
		if point.ExpenseMinor != nil {
			value = point.GetExpenseMinor()
		}
		points = append(points, p.barPoint(point, value))
		v := "нет данных"
		if point.ExpenseMinor != nil {
			v = money(point.GetExpenseMinor())
		}
		alt = append(alt, monthName(point.Month, monthNom)+" "+v)
	}
	d := map[string]any{"kind": "bar", "value_kind": "amount", "points": points, "notes": p.chartNotes(chart.Points)}
	if p.s.Key.Type != domain.PeriodYear && len(points) > 0 {
		d["highlight_index"] = len(points) - 1
	}
	b.AltText = ptr(title(id) + ": " + strings.Join(alt, ", "))
	b.Data, err = data(d)
	return b, err
}

func (p composition) balanceChart(ctx context.Context, chartMetric *analyticsv1.Metric) (*commonv1.Block, error) {
	m, err := p.get("tiles.balance")
	if err != nil {
		return nil, err
	}
	b := p.base("balance", m)
	if !ready(m) {
		return b, nil
	}
	points := []any{}
	withChart := ready(chartMetric) && chartMetric.GetMonthlyChart() != nil
	if withChart {
		for _, point := range chartMetric.GetMonthlyChart().Points {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if point.GetRange().GetStart() == nil || point.GetRange().GetEnd() == nil {
				// A malformed optional graph must not suppress its independent balance summary.
				points = []any{}
				withChart = false
				break
			}
			var value any
			if point.IncomeMinor != nil && point.ExpenseMinor != nil && point.GetIncomeMinor() > 0 {
				value = (float64(point.GetIncomeMinor()) - float64(point.GetExpenseMinor())) / float64(point.GetIncomeMinor())
			}
			points = append(points, p.barPoint(point, value))
		}
	}
	caption := "Доходов за период нет"
	if detail := m.GetBalance(); detail != nil && detail.ShareOfIncome != nil {
		pct := int64(math.Floor(detail.GetShareOfIncome()*100 + 0.5))
		caption = fmt.Sprintf("%d%% дохода: на столько доходы по выписке больше расходов", pct)
		if detail.GetShareOfIncome() < 0 {
			caption = fmt.Sprintf("%d%% дохода: на столько расходы по выписке больше доходов", -pct)
		}
	}
	if withChart {
		caption += ". На графике — та же доля по месяцам"
	}
	d := map[string]any{"kind": "bar", "value_kind": "percent", "points": points, "show_values": true, "caption": caption}
	if m.ValueMinor != nil {
		d["summary"] = map[string]any{"value": m.GetValueMinor(), "value_kind": "amount"}
	}
	b.AltText = ptr(altParts(title("balance"), money(m.GetValueMinor()), caption))
	b.Data, err = data(d)
	return b, err
}

func (p composition) chartNotes(points []*analyticsv1.MonthlyPoint) []any {
	notes := []any{}
	if p.s.Key.Type == domain.PeriodMonth {
		actual, e1 := p.get("tiles.expense")
		previous, e2 := p.get("tiles.expense.comparison")
		if e1 == nil && e2 == nil && ready(actual) && ready(previous) && actual.ValueMinor != nil && previous.ValueMinor != nil {
			text := "В прошлом периоде к этому дню расходов не было"
			if previous.GetValueMinor() != 0 {
				pct := math.Floor((float64(actual.GetValueMinor())-float64(previous.GetValueMinor()))*100/math.Abs(float64(previous.GetValueMinor())) + 0.5)
				cmp := "столько же, сколько"
				if pct > 0 {
					cmp = fmt.Sprintf("на %.0f%% больше, чем", pct)
				}
				if pct < 0 {
					cmp = fmt.Sprintf("на %.0f%% меньше, чем", -pct)
				}
				text = fmt.Sprintf("Расходы %s — %s прошлый период к тому же дню", money(actual.GetValueMinor()), cmp)
			}
			notes = append(notes, map[string]any{"text": text, "tone": "primary"})
		}
	} else {
		var peak *analyticsv1.MonthlyPoint
		for _, point := range points {
			if point.ExpenseMinor != nil && (peak == nil || point.GetExpenseMinor() > peak.GetExpenseMinor()) {
				peak = point
			}
		}
		if peak != nil {
			notes = append(notes, map[string]any{"text": "Больше всего — в " + monthName(peak.Month, monthPrep) + ": " + money(peak.GetExpenseMinor()), "tone": "primary"})
		}
	}
	if p.s.Key.Type == domain.PeriodYear {
		parts := []string{}
		last := len(points) - 1
		if p.s.Current {
			for last >= 0 && points[last].ExpenseMinor == nil {
				last--
			}
		}
		for i, point := range points {
			if i > last {
				break
			}
			if point.ExpenseMinor == nil {
				parts = append(parts, monthName(point.Month, monthNom)+": в выписке нет операций")
			} else if note := p.barNote(point); note != "" {
				parts = append(parts, monthName(point.Month, monthNom)+" — "+note)
			}
		}
		if len(parts) > 0 {
			notes = append(notes, map[string]any{"text": strings.Join(parts, ". "), "tone": "muted"})
		}
	} else {
		typical, err := p.get("tiles.expense.typical")
		if err == nil && ready(typical) && typical.ValueMinor != nil {
			text := "Обычно около " + money(typical.GetValueMinor())
			if basis := monthRange(typical.GetBasis().GetRange()); basis != "" {
				text += " — " + basis
			}
			notes = append(notes, map[string]any{"text": text, "tone": "muted"})
		}
	}
	return notes
}
