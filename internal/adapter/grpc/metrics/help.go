package metrics

import (
	"fmt"
	"strings"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

const dailyExpenseTileID = "daily_expense"
const dailyExpenseHelp = "По дням с данными не позже сегодня; пропуски не учитываются"

// The compact lock stays visible; coverage and methodology are explicitly auxiliary.
func (p composition) lock(id string, l *analyticsv1.MetricLock) *commonv1.BlockLock {
	out := p.detailedLock(id, l)
	if l.GetReason() == analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS && l.Required != nil {
		out.Text = fmt.Sprintf("Нужно %d %s", l.GetRequired(), noun(int64(l.GetRequired()), "полный месяц", "полных месяца", "полных месяцев"))
		if l.Available != nil {
			out.Text += fmt.Sprintf(", есть %d", l.GetAvailable())
		}
	}
	return out
}

func (p composition) help(id string, m *analyticsv1.Metric) string {
	parts := []string{}
	if id == dailyExpenseTileID && ready(m) {
		parts = append(parts, dailyExpenseHelp)
	}
	if metricStatus(m) == "locked" {
		detail, short := p.detailedLock(id, m.GetLock()).Text, p.lock(id, m.GetLock()).Text
		if detail != short {
			parts = append(parts, detail)
		}
	}
	if id == "forecast" {
		switch p.s.Key.Type {
		case domain.PeriodYear:
			parts = append(parts, "Это оценка расходов за весь год. К расходам с начала года добавляем прогноз на оставшиеся дни по средним дневным расходам за последние 12 полных месяцев загруженной истории.")
		case domain.PeriodQuarter:
			parts = append(parts, "Оценка расходов к концу квартала по средним дневным расходам за последние 3 полных месяца загруженной истории.")
		}
	}
	return strings.Join(parts, " ")
}
