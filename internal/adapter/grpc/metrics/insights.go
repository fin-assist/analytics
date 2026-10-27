package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fin-assist/analytics/internal/domain"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
)

func (p composition) insight(ctx context.Context, id string) ([]*commonv1.Block, error) {
	m, err := p.get("insights." + id)
	if err != nil {
		return nil, err
	}
	b := p.base(id, m)
	b.Action = blockAction(m.Filters, p.zone)
	d := map[string]any{"icon": icon(id), "title": title(id)}
	if !ready(m) {
		if metricStatus(m) == "none" {
			b.EmptyText = ptr(p.emptyInsight(id, m))
			b.AltText = b.EmptyText
		}
		b.Action = nil
		b.Data, err = data(d)
		return []*commonv1.Block{b}, err
	}
	if m.ValueMinor != nil {
		d["value"] = m.GetValueMinor()
		d["value_kind"] = "amount"
	}
	if metricStatus(m) == "tentative" {
		d["badge"] = "предварительно"
	}
	switch id {
	case "regular_payments":
		r := m.GetRegularPayments()
		if r == nil {
			return nil, errors.New("regular payment details absent")
		}
		description := "в месяц"
		if r.Count != nil {
			description += fmt.Sprintf(" · %d %s каждый месяц", r.GetCount(), noun(int64(r.GetCount()), "списание", "списания", "списаний"))
		}
		if m.GetBasis().GetRange().GetStart() != nil {
			description += " с " + monthGen[m.Basis.Range.Start.AsTime().In(p.zone).Month()-1]
		}
		d["description"] = description
		items := []any{}
		var rest int64
		for i, row := range r.Items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if i >= 3 {
				if row.MonthlyAmountMinor > 1<<53-1 || row.MonthlyAmountMinor < -(1<<53-1) || rest > 1<<53-1-row.MonthlyAmountMinor || rest < -(1<<53-1)-row.MonthlyAmountMinor {
					return nil, errors.New("regular remainder exceeds exact range")
				}
				rest += row.MonthlyAmountMinor
				continue
			}
			item := map[string]any{"title": row.Title, "sublabel": row.ScheduleName, "value": row.AmountMinor, "value_kind": "amount"}
			if a := actionJSON(row.Filters, p.zone); a != nil {
				item["action"] = a
			}
			if domain.ValidUserID(row.Id) {
				item["button"] = map[string]any{"type": "dismiss_regular_payment", "payment_id": row.Id, "label": "Не подписка"}
			}
			items = append(items, item)
		}
		d["items"] = items
		if len(r.Items) > 3 {
			d["items_more"] = map[string]any{"label": fmt.Sprintf("Ещё %d", len(r.Items)-3), "value": rest, "value_kind": "amount"}
		}
	case "notable_spending":
		r := m.GetNotableSpending()
		if r == nil || len(r.Items) == 0 {
			return nil, errors.New("notable spending details absent")
		}
		out := make([]*commonv1.Block, 0, len(r.Items))
		for _, row := range r.Items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			one := p.base(id, m)
			one.Title = ptr(row.CategoryName)
			description := "Обычно около " + money(row.TypicalAmountMinor)
			if row.TypicalAmountMinor > 0 {
				description = fmt.Sprintf("В %.1f раза больше обычного — обычно около %s", float64(row.AmountMinor)/float64(row.TypicalAmountMinor), money(row.TypicalAmountMinor))
				description = strings.Replace(description, ".", ",", 1)
			}
			if basis := monthRange(m.GetBasis().GetRange(), p.zone); basis != "" {
				description += " (" + basis + ")"
			}
			one.Data, err = data(map[string]any{"icon": row.CategoryIcon, "title": row.CategoryName, "value": row.AmountMinor, "value_kind": "amount", "description": description})
			if err != nil {
				return nil, err
			}
			one.Action = blockAction(row.Filters, p.zone)
			one.AltText = ptr(altParts(row.CategoryName, money(row.AmountMinor), description))
			out = append(out, one)
		}
		return out, nil
	case "bank_fees":
		if m.GetBankFees() == nil {
			return nil, errors.New("bank fees details absent")
		}
		description := "за " + p.caption()
		if m.GetBankFees().KindsName != nil {
			description += " · " + m.GetBankFees().GetKindsName()
		}
		d["description"] = description
		b.Action = blockAction(m.Filters, p.zone)
	case "small_frequent":
		r := m.GetSmallFrequent()
		if r == nil {
			return nil, errors.New("small frequent details absent")
		}
		d["approx"] = true
		items := []any{}
		for i, row := range r.Items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			item := map[string]any{"title": row.Title, "sublabel": fmt.Sprintf("%d %s в месяц · по %s", row.Count, noun(int64(row.Count), "раз", "раза", "раз"), money(row.AverageAmountMinor)), "value": row.MonthlyAmountMinor, "value_kind": "amount", "value_suffix": "в месяц"}
			if a := actionJSON(row.Filters, p.zone); a != nil {
				item["action"] = a
			}
			if i > 0 {
				items = append(items, item)
			}
			if i == 0 {
				b.Action = blockAction(row.Filters, p.zone)
				description := fmt.Sprintf("в месяц · %s: %d %s по %s — около %s в год", row.Title, row.Count, noun(int64(row.Count), "раз", "раза", "раз"), money(row.AverageAmountMinor), money(row.YearlyAmountMinor))
				if m.GetBasis().GetRange().GetStart() != nil {
					base := m.Basis.Range.Start.AsTime().In(p.zone)
					if base.Format("2006-01") != p.s.Key.Key {
						prefix := "по " + monthDat[base.Month()-1]
						if base.Year() != p.s.Range.Start.In(p.zone).Year() {
							prefix += fmt.Sprintf(" %d", base.Year())
						}
						description = prefix + " · " + description
					}
				}
				d["description"] = description
			}
		}
		d["items"] = items
	}
	b.Data, err = data(d)
	description, _ := d["description"].(string)
	b.AltText = ptr(altParts(title(id), money(m.GetValueMinor()), description))
	return []*commonv1.Block{b}, err
}

func (p composition) emptyInsight(id string, m *analyticsv1.Metric) string {
	switch id {
	case "regular_payments":
		text := "Регулярных платежей не нашли"
		if basis := monthRange(m.GetBasis().GetRange(), p.zone); basis != "" {
			text += " за " + basis
		}
		return text
	case "notable_spending":
		return "Заметных трат не нашли — расходы по категориям в пределах обычного"
	case "bank_fees":
		return "Комиссий за " + p.caption() + " нет"
	default:
		return "Мелких частых трат не нашли"
	}
}
