package metrics

import (
	"fmt"
	"strings"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

var monthGen = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
var monthNom = [...]string{"январь", "февраль", "март", "апрель", "май", "июнь", "июль", "август", "сентябрь", "октябрь", "ноябрь", "декабрь"}
var monthPrep = [...]string{"январе", "феврале", "марте", "апреле", "мае", "июне", "июле", "августе", "сентябре", "октябре", "ноябре", "декабре"}
var monthDat = [...]string{"январю", "февралю", "марту", "апрелю", "маю", "июню", "июлю", "августу", "сентябрю", "октябрю", "ноябрю", "декабрю"}

func noun(n int64, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}
func day(t time.Time) string { return fmt.Sprintf("%d %s", t.Day(), monthGen[t.Month()-1]) }
func dayRange(from, to time.Time) string {
	if from.Year() != to.Year() {
		return fmt.Sprintf("%s %d — %s %d", day(from), from.Year(), day(to), to.Year())
	}
	if from.Month() != to.Month() {
		return day(from) + " — " + day(to)
	}
	if from.Day() == to.Day() {
		return day(from)
	}
	return fmt.Sprintf("%d–%s", from.Day(), day(to))
}
func monthRange(r *commonv1.TimeRange, zone *time.Location) string {
	if r.GetStart() == nil || r.GetEnd() == nil {
		return ""
	}
	from, to := r.Start.AsTime().In(zone), r.End.AsTime().Add(-time.Nanosecond).In(zone)
	if from.Year() != to.Year() {
		return fmt.Sprintf("%s %d — %s %d", monthNom[from.Month()-1], from.Year(), monthNom[to.Month()-1], to.Year())
	}
	if from.Month() == to.Month() {
		return monthNom[from.Month()-1]
	}
	return monthNom[from.Month()-1] + " — " + monthNom[to.Month()-1]
}
func dateString(t *timestamppb.Timestamp, zone *time.Location) any {
	if t == nil {
		return nil
	}
	return t.AsTime().In(zone).Format(time.RFC3339Nano)
}
func rangeJSON(r *commonv1.TimeRange, zone *time.Location) map[string]any {
	return map[string]any{"from": dateString(r.GetStart(), zone), "to": dateString(r.GetEnd(), zone)}
}

func actionJSON(f *commonv1.OperationsFilter, zone *time.Location) map[string]any {
	if f == nil {
		return nil
	}
	out := map[string]any{}
	if f.From != nil {
		out["from"] = dateString(f.From, zone)
	}
	if f.To != nil {
		out["to"] = dateString(f.To, zone)
	}
	if f.Q != nil && *f.Q != "" {
		out["q"] = *f.Q
	}
	if f.CategoryId != nil && *f.CategoryId != "" {
		out["category_id"] = *f.CategoryId
	}
	switch f.Kind {
	case commonv1.KindFilter_KIND_FILTER_EXPENSE:
		out["kind"] = "expense"
	case commonv1.KindFilter_KIND_FILTER_INCOME:
		out["kind"] = "income"
	case commonv1.KindFilter_KIND_FILTER_UNSPECIFIED:
	default:
		return nil
	}
	if f.AmountFromMinor != nil {
		out["amount_from"] = *f.AmountFromMinor
	}
	if f.AmountToMinor != nil {
		out["amount_to"] = *f.AmountToMinor
	}
	if f.Selection != nil && *f.Selection != "" {
		out["selection"] = *f.Selection
	}
	if len(out) == 0 {
		return nil
	}
	switch f.TransferMode {
	case commonv1.TransferMode_TRANSFER_MODE_WITH:
		out["transfer_mode"] = "with"
	case commonv1.TransferMode_TRANSFER_MODE_WITHOUT:
		out["transfer_mode"] = "without"
	case commonv1.TransferMode_TRANSFER_MODE_UNSPECIFIED:
	default:
		return nil
	}
	if f.SelectionName != nil {
		out["selection_name"] = *f.SelectionName
	}
	return map[string]any{"type": "open_operations", "filter": out}
}
func blockAction(f *commonv1.OperationsFilter, zone *time.Location) *commonv1.BlockAction {
	if actionJSON(f, zone) == nil {
		return nil
	}
	return &commonv1.BlockAction{Type: "open_operations", Filter: f}
}
func lockJSON(l *commonv1.BlockLock, zone *time.Location) map[string]any {
	out := map[string]any{"reason": l.Reason, "text": l.Text}
	if l.Required != nil {
		out["required"] = l.GetRequired()
	}
	if l.Available != nil {
		out["available"] = l.GetAvailable()
	}
	if l.AvailableFrom != nil {
		out["available_from"] = l.AvailableFrom.AsTime().In(zone).Format(time.RFC3339Nano)
	}
	return out
}
func coverage(c commonv1.Coverage) string {
	switch c {
	case commonv1.Coverage_COVERAGE_COMPLETE:
		return "complete"
	case commonv1.Coverage_COVERAGE_NO_DATA:
		return "no_data"
	default:
		return "partial"
	}
}
func (p composition) barPoint(point *analyticsv1.MonthlyPoint, value any) map[string]any {
	out := map[string]any{"range": rangeJSON(point.GetRange(), p.zone), "coverage": coverage(point.GetBounds().GetCoverage())}
	if value != nil {
		out["value"] = value
	}
	if point.GetBounds().GetCoverage() == commonv1.Coverage_COVERAGE_PARTIAL {
		if point.Bounds.DataFrom != nil {
			out["data_from"] = dateString(point.Bounds.DataFrom, p.zone)
		}
		if point.Bounds.DataTo != nil {
			out["data_to"] = dateString(point.Bounds.DataTo, p.zone)
		}
		if note := p.barNote(point); note != "" {
			out["note"] = note
		}
	}
	return out
}
func (p composition) barNote(point *analyticsv1.MonthlyPoint) string {
	b := point.GetBounds()
	if b.GetCoverage() != commonv1.Coverage_COVERAGE_PARTIAL {
		return ""
	}
	if len(b.Gaps) > 0 {
		g := b.Gaps[0]
		if g.GetStart() != nil && g.GetEnd() != nil {
			return "без " + dayRange(g.Start.AsTime().In(p.zone), g.End.AsTime().Add(-time.Nanosecond).In(p.zone))
		}
	}
	if b.DataTo != nil && point.GetRange().GetEnd() != nil {
		to, end := b.DataTo.AsTime().In(p.zone), point.Range.End.AsTime().Add(-time.Nanosecond).In(p.zone)
		if to.Day() < end.Day() {
			return fmt.Sprintf("по %d-е", to.Day())
		}
	}
	if b.DataFrom != nil && b.DataFrom.AsTime().In(p.zone).Day() > 1 {
		return fmt.Sprintf("с %d-го", b.DataFrom.AsTime().In(p.zone).Day())
	}
	return ""
}

func altParts(parts ...string) string {
	out := []string{}
	for _, s := range parts {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ": ")
}

func (p composition) forecastCaption() string {
	switch p.s.Key.Type {
	case domain.PeriodMonth:
		return "на " + monthNom[p.s.Range.Start.In(p.zone).Month()-1]
	case domain.PeriodQuarter:
		return "на " + [...]string{"I", "II", "III", "IV"}[(p.s.Range.Start.In(p.zone).Month()-1)/3] + " квартал"
	default:
		return fmt.Sprintf("на %d год", p.s.Range.Start.In(p.zone).Year())
	}
}
