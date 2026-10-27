package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/fin-assist/analytics/internal/domain"
)

const unavailableText = "Не получилось посчитать — обновите экран позже"
const blockTimeout = 250 * time.Millisecond

// Composer builds the fixed MVP composition. Labels are the bounded server-owned block IDs.
type Composer struct{ attempts, unavailable *prometheus.CounterVec }

// NewComposer registers counters for the unavailable share of each composition block.
func NewComposer(reg prometheus.Registerer) (*Composer, error) {
	if reg == nil {
		return nil, errors.New("composer: registry required")
	}
	c := &Composer{
		attempts:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_analytics_blocks_total", Help: "Analytics block composition attempts."}, []string{"block_id"}),
		unavailable: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_analytics_blocks_unavailable_total", Help: "Unavailable analytics block compositions."}, []string{"block_id"}),
	}
	if err := reg.Register(c.attempts); err != nil {
		return nil, err
	}
	if err := reg.Register(c.unavailable); err != nil {
		reg.Unregister(c.attempts)
		return nil, err
	}
	return c, nil
}

type composition struct {
	s       domain.Analytics
	zone    *time.Location
	metrics map[string]domain.Metric
}

func (p composition) get(id string) (*analyticsv1.Metric, error) {
	m, ok := p.metrics[id]
	if !ok {
		return &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_UNAVAILABLE}, nil
	}
	return metric(m)
}

// Compose projects persisted aggregates and derives only the display quantities of
// calc.md 1/5/10 and api.md 6.1. It never reaggregates operations or chooses bases/thresholds.
func (c *Composer) Compose(ctx context.Context, s domain.Analytics, supports []*commonv1.BlockSupport, exp map[string]string) (*analyticsv1.GetAnalyticsResponse, error) {
	zone, err := time.LoadLocation(s.State.TimeZone)
	if err != nil {
		return nil, err
	}
	p := composition{s: s, zone: zone, metrics: map[string]domain.Metric{}}
	if s.Snapshot != nil {
		for _, m := range s.Snapshot.Metrics {
			p.metrics[m.ID] = m
		}
	}
	bounds := &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_NO_DATA}
	state := &analyticsv1.ScreenState{Stale: s.Stale, LastOperationAt: timestamp(s.State.LastOperationAt), DataFrom: timestamp(s.State.DataFrom), DataTo: timestamp(s.State.DataTo), FullMonths: s.State.FullMonths, CalculatedAt: timestamp(s.State.CalculatedAt), Recalculating: s.State.Recalculating}
	if snap := s.Snapshot; snap != nil {
		bounds = &commonv1.DataBounds{Coverage: commonv1.Coverage(snap.Coverage), DataFrom: timestamp(snap.DataFrom), DataTo: timestamp(snap.DataTo)}
		for _, gap := range snap.Gaps {
			bounds.Gaps = append(bounds.Gaps, timerange(&gap))
		}
		state.UnreadLinesCount = snap.UnreadLines
		state.CalculatedAt = timestamp(snap.CalculatedAt)
	}
	out := &analyticsv1.GetAnalyticsResponse{HasData: true, Currency: "RUB", TimeZone: s.State.TimeZone,
		Params: &commonv1.AnalyticsParams{Period: commonv1.PeriodType(s.Key.Type), PeriodKey: s.Key.Key, TransferMode: commonv1.TransferMode(s.Key.Mode)},
		Period: &analyticsv1.PeriodInfo{Range: timerange(&s.Range), IsCurrent: s.Current, Bounds: bounds}, Navigation: &analyticsv1.Navigation{Previous: s.Previous, Next: s.Next}, State: state}
	ids := []string{"tiles", "monthly_chart", "expense_categories", "income_categories"}
	if exp["analytics_order"] == "b" {
		ids = []string{"expense_categories", "income_categories", "tiles", "monthly_chart"}
	}
	if s.Key.Type == domain.PeriodMonth {
		ids = append(ids, "heading", "regular_payments", "notable_spending", "bank_fees", "small_frequent")
	}
	if s.Key.Type != domain.PeriodYear {
		ids = append(ids, "balance")
	}
	available, attempted := 0, 0
	for _, id := range ids {
		if id == "heading" && !compatible("insight_card", supports) {
			continue
		}
		if !compatible(blockType(id), supports) {
			continue
		}
		if id == "monthly_chart" && p.early() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		blocks, err := guardBlock(ctx, func(ctx context.Context) ([]*commonv1.Block, error) { return p.build(ctx, id) })
		if err != nil {
			blocks = []*commonv1.Block{unavailableBlock(id)}
		}
		if id != "heading" {
			attempted++
			c.attempts.WithLabelValues(id).Inc()
			failed := true
			for _, b := range blocks {
				if b.Status != "unavailable" {
					failed = false
				}
			}
			if failed {
				c.unavailable.WithLabelValues(id).Inc()
			}
			if err == nil {
				available++
			}
		}
		out.Blocks = append(out.Blocks, blocks...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if attempted > 0 && available == 0 {
		return nil, errors.New("analytics: no block could be composed")
	}
	return out, nil
}

func compatible(typ string, supports []*commonv1.BlockSupport) bool {
	if len(supports) == 0 {
		return true
	}
	for _, s := range supports {
		if s.GetType() == typ && s.GetMaxVersion() >= 1 {
			return true
		}
	}
	return false
}

type blockResult struct {
	blocks []*commonv1.Block
	err    error
}

func guardBlock(ctx context.Context, build func(context.Context) ([]*commonv1.Block, error)) ([]*commonv1.Block, error) {
	ctx, cancel := context.WithTimeout(ctx, blockTimeout)
	defer cancel()
	done := make(chan blockResult, 1)
	go func() { b, e := build(ctx); done <- blockResult{b, e} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.blocks, r.err
	}
}

func blockType(id string) string {
	switch id {
	case "tiles":
		return "tile_group"
	case "monthly_chart", "balance":
		return "chart"
	case "expense_categories", "income_categories":
		return "ranked_list"
	case "heading":
		return "heading"
	default:
		return "insight_card"
	}
}
func title(id string) string {
	switch id {
	case "tiles":
		return "Сводные показатели"
	case "monthly_chart":
		return "Расходы по месяцам"
	case "expense_categories":
		return "Расходы по категориям"
	case "income_categories":
		return "Доходы по категориям"
	case "regular_payments":
		return "Подписки и регулярные платежи"
	case "notable_spending":
		return "Заметные траты"
	case "bank_fees":
		return "Комиссии и проценты банку"
	case "small_frequent":
		return "Мелкие частые траты"
	default:
		return "Доходы минус расходы"
	}
}
func icon(id string) string {
	switch id {
	case "regular_payments":
		return "repeat"
	case "bank_fees":
		return "percent"
	case "small_frequent":
		return "coffee"
	default:
		return "bar-chart"
	}
}
func unavailableBlock(id string) *commonv1.Block {
	b := &commonv1.Block{Id: id, Type: blockType(id), V: 1, Status: "unavailable", Title: ptr(title(id)), AltText: ptr(unavailableText), EmptyText: ptr(unavailableText)}
	if b.Type == "insight_card" {
		b.Data, _ = data(map[string]any{"icon": icon(id), "title": title(id)})
	}
	if b.Type == "tile_group" {
		b.Data, _ = data(map[string]any{"tiles": []any{}})
	}
	return b
}
func ptr[T any](v T) *T { return &v }

func metricStatus(m *analyticsv1.Metric) string {
	switch m.GetStatus() {
	case analyticsv1.MetricStatus_METRIC_STATUS_READY:
		return "ready"
	case analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE:
		return "tentative"
	case analyticsv1.MetricStatus_METRIC_STATUS_LOCKED:
		return "locked"
	case analyticsv1.MetricStatus_METRIC_STATUS_NONE:
		return "none"
	default:
		return "unavailable"
	}
}
func ready(m *analyticsv1.Metric) bool {
	return metricStatus(m) == "ready" || metricStatus(m) == "tentative"
}

// data checks exact JSON integer representation before Struct converts numbers to double.
func data(v map[string]any) (*structpb.Struct, error) {
	if err := safeNumbers(v); err != nil {
		return nil, err
	}
	return structpb.NewStruct(v)
}
func safeNumbers(v any) error {
	switch n := v.(type) {
	case int64:
		if n > 1<<53-1 || n < -(1<<53-1) {
			return errors.New("amount exceeds exact JSON integer range")
		}
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return errors.New("non-finite value")
		}
	case map[string]any:
		for _, x := range n {
			if err := safeNumbers(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range n {
			if err := safeNumbers(x); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p composition) early() bool {
	m, err := p.get("monthly_chart")
	return err == nil && p.s.Key.Type == domain.PeriodMonth && p.s.Snapshot != nil && p.s.Snapshot.Coverage == domain.CoveragePartial && m.GetLock().GetReason() == analyticsv1.LockReason_LOCK_REASON_NEED_MONTHS_WITH_DATA && m.GetLock().GetAvailable() == 1
}

func (p composition) build(ctx context.Context, id string) ([]*commonv1.Block, error) {
	if id == "heading" {
		d, err := data(map[string]any{"text": "Регулярные платежи и заметные траты"})
		return []*commonv1.Block{{Type: "heading", V: 1, Data: d}}, err
	}
	if id == "tiles" {
		b, e := p.tiles(ctx)
		return []*commonv1.Block{b}, e
	}
	if id == "monthly_chart" || id == "balance" {
		b, e := p.chart(ctx, id)
		return []*commonv1.Block{b}, e
	}
	if id == "expense_categories" || id == "income_categories" {
		b, e := p.categories(ctx, id)
		return []*commonv1.Block{b}, e
	}
	return p.insight(ctx, id)
}

func (p composition) base(id string, m *analyticsv1.Metric) *commonv1.Block {
	b := &commonv1.Block{Id: id, Type: blockType(id), V: 1, Title: ptr(title(id)), AltText: ptr(title(id)), Status: metricStatus(m)}
	if b.Status == "unavailable" {
		b.EmptyText = ptr(unavailableText)
	}
	if b.Status == "locked" {
		b.Lock = p.lock(id, m.GetLock())
		b.AltText = ptr(title(id) + ": " + b.Lock.Text)
	}
	return b
}

func (p composition) lock(id string, l *analyticsv1.MetricLock) *commonv1.BlockLock {
	out := &commonv1.BlockLock{Text: "Пока недоступно"}
	if l == nil {
		return out
	}
	out.Required = l.Required
	out.Available = l.Available
	out.AvailableFrom = l.AvailableFrom
	switch l.Reason {
	case analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS:
		out.Reason = "need_full_months"
		out.Text = "Нужно больше полных месяцев"
		if l.Required != nil {
			out.Text = fmt.Sprintf("Нужно %d %s", l.GetRequired(), noun(int64(l.GetRequired()), "полный месяц", "полных месяца", "полных месяцев"))
			if l.GetAvailable() == 0 {
				from, to := p.s.State.DataFrom, p.s.State.DataTo
				// Windowed observations may only describe the data before the selected boundary.
				limit := p.s.Range.End
				if id == "notable_spending" {
					limit = p.s.Range.Start
				}
				if (id == "regular_payments" || id == "notable_spending" || id == "small_frequent") && to != nil && limit != nil && to.After(*limit) {
					to = limit
				}
				if from != nil && to != nil && from.Before(*to) {
					out.Text += " — пока есть только " + dayRange(from.In(p.zone), to.Add(-time.Nanosecond).In(p.zone))
				} else {
					out.Text += ", есть 0"
				}
			} else {
				out.Text += fmt.Sprintf(", есть %d", l.GetAvailable())
			}
		}
	case analyticsv1.LockReason_LOCK_REASON_NEED_MONTHS_WITH_DATA:
		out.Reason = "need_months_with_data"
		out.Text = fmt.Sprintf("Нужно %d %s с данными, есть %d", l.GetRequired(), noun(int64(l.GetRequired()), "месяц", "месяца", "месяцев"), l.GetAvailable())
	case analyticsv1.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH:
		out.Reason = "too_early_in_month"
		out.Text = "Рано считать — мало дней"
		if l.AvailableFrom != nil {
			out.Text = "Посчитаем с " + day(l.AvailableFrom.AsTime().In(p.zone))
		}
	case analyticsv1.LockReason_LOCK_REASON_STALE_DATA:
		out.Reason = "stale_data"
		out.Text = "Выписка устарела — загрузите новую"
	}
	return out
}

func (p composition) caption() string {
	from, to := *p.s.Range.Start, p.s.Range.End.Add(-time.Nanosecond)
	if p.s.Snapshot != nil {
		from = *p.s.Snapshot.DataFrom
		to = *p.s.Snapshot.DataTo
	}
	return dayRange(from.In(p.zone), to.In(p.zone))
}

func (p composition) tile(id, label string, m *analyticsv1.Metric, caption string) map[string]any {
	out := map[string]any{"id": id, "title": label, "status": metricStatus(m)}
	if ready(m) && m.ValueMinor == nil {
		out["status"] = "unavailable"
	}
	if metricStatus(m) == "locked" {
		out["lock"] = lockJSON(p.lock(id, m.GetLock()), p.zone)
	}
	if ready(m) && m.ValueMinor != nil {
		out["value"] = m.GetValueMinor()
		out["value_kind"] = "amount"
		if caption != "" {
			out["caption"] = caption
		}
		if a := actionJSON(m.GetFilters(), p.zone); a != nil {
			out["action"] = a
		}
	}
	return out
}

func (p composition) tiles(ctx context.Context) (*commonv1.Block, error) {
	ids := []string{"tiles.balance", "tiles.expense", "tiles.daily_expense", "tiles.income", "tiles.forecast", "tiles.daily_expense.comparison"}
	values := map[string]*analyticsv1.Metric{}
	for _, id := range ids {
		m, err := p.get(id)
		if err != nil {
			return nil, err
		}
		values[id] = m
	}
	bal, expense, daily, income := values[ids[0]], values[ids[1]], values[ids[2]], values[ids[3]]
	share := "Доходов за период нет"
	if detail := bal.GetBalance(); detail != nil && detail.ShareOfIncome != nil {
		pct := int64(math.Floor(*detail.ShareOfIncome*100 + 0.5))
		share = fmt.Sprintf("%d%% дохода · по выписке", pct)
		if pct < 0 {
			share = fmt.Sprintf("−%d%% · потратили больше, чем получили", -pct)
		}
	}
	second := p.tile("expense", "Расходы", expense, p.caption())
	forecast := values["tiles.forecast"]
	if p.s.Current && !p.s.Stale && forecast.GetLock().GetReason() != analyticsv1.LockReason_LOCK_REASON_STALE_DATA {
		label := "Прогноз расходов"
		if p.s.Key.Type == domain.PeriodYear {
			label = "Прогноз на год"
		}
		second = p.tile("forecast", label, forecast, p.forecastCaption())
		second["approx"] = true
	}
	dailyCaption := "в среднем за " + p.caption()
	cmp := values["tiles.daily_expense.comparison"]
	if ready(cmp) && cmp.ValueMinor != nil {
		dailyCaption = "раньше — " + money(cmp.GetValueMinor())
		if cmp.GetRange().GetStart() != nil {
			dailyCaption = "в " + monthPrep[cmp.Range.Start.AsTime().In(p.zone).Month()-1] + " — " + money(cmp.GetValueMinor())
		}
	}
	tiles := []any{p.tile("balance", "Доходы минус расходы", bal, share), second, p.tile("daily_expense", "Расходы в день", daily, dailyCaption), p.tile("income", "Доходы", income, "")}
	tiles[3].(map[string]any)["show_plus"] = true
	b := &commonv1.Block{Id: "tiles", Type: "tile_group", V: 1, Status: "ready", AltText: ptr("Сводные показатели за " + p.caption())}
	allMissing := true
	for _, t := range tiles {
		if t.(map[string]any)["status"] != "unavailable" {
			allMissing = false
		}
	}
	if allMissing {
		b = unavailableBlock("tiles")
	}
	alts := []string{}
	for _, item := range tiles {
		t := item.(map[string]any)
		text := t["title"].(string)
		if n, ok := t["value"].(int64); ok {
			text += " " + money(n)
		} else if l, ok := t["lock"].(map[string]any); ok {
			text += " — " + l["text"].(string)
		} else {
			text += " — пока недоступно"
		}
		alts = append(alts, text)
	}
	b.AltText = ptr(strings.Join(alts, ", "))
	var err error
	b.Data, err = data(map[string]any{"tiles": tiles})
	if err != nil {
		return nil, err
	}
	return b, ctx.Err()
}

func (p composition) categories(ctx context.Context, id string) (*commonv1.Block, error) {
	m, err := p.get(id)
	if err != nil {
		return nil, err
	}
	b := p.base(id, m)
	if !ready(m) {
		if metricStatus(m) == "none" {
			b.EmptyText = ptr(title(id) + ": за " + p.caption() + " нет")
		}
		return b, nil
	}
	if m.GetCategories() == nil {
		return nil, errors.New("category details absent")
	}
	items := []any{}
	for _, row := range m.GetCategories().Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := map[string]any{"icon": row.CategoryIcon, "title": row.CategoryName, "value": row.AmountMinor, "value_kind": "amount"}
		if row.CategoryId != nil {
			item["id"] = *row.CategoryId
		}
		if row.Note != nil {
			item["note"] = *row.Note
		} else {
			item["note"] = fmt.Sprintf("%d %s", row.OperationCount, noun(row.OperationCount, "операция", "операции", "операций"))
		}
		if row.Share != nil {
			item["share"] = *row.Share
		}
		if a := actionJSON(row.Filters, p.zone); a != nil {
			item["action"] = a
		}
		if id == "income_categories" {
			item["show_plus"] = true
		}
		items = append(items, item)
	}
	d := map[string]any{"items": items, "visible_count": 4, "more_label": "Все категории"}
	totalID := "tiles.expense"
	if id == "income_categories" {
		totalID = "tiles.income"
		d["style"] = "plain"
	}
	total, err := p.get(totalID)
	if err != nil {
		return nil, err
	}
	if ready(total) && total.ValueMinor != nil {
		v := map[string]any{"label": "Всего · " + p.caption(), "value": total.GetValueMinor(), "value_kind": "amount"}
		if a := actionJSON(total.Filters, p.zone); a != nil {
			v["action"] = a
		}
		d["total"] = v
	}
	alts := []string{}
	for _, item := range items {
		r := item.(map[string]any)
		alts = append(alts, r["title"].(string)+" "+money(r["value"].(int64)))
	}
	b.AltText = ptr(title(id) + ": " + strings.Join(alts, ", "))
	b.Data, err = data(d)
	return b, err
}

func money(v int64) string {
	// All callers validate the Struct amount independently; MinInt64 is safe here too.
	s := strconv.FormatInt(v, 10)
	sign := ""
	if len(s) > 0 && s[0] == '-' {
		sign = "−"
		s = s[1:]
	}
	for len(s) < 3 {
		s = "0" + s
	}
	fraction := s[len(s)-2:]
	whole := s[:len(s)-2]
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + " " + whole[i:]
	}
	if fraction != "00" {
		whole += "," + fraction
	}
	return sign + whole + " ₽"
}
