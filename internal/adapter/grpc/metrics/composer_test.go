package metrics

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/domain"
)

func screenFixture(t *testing.T) domain.Analytics {
	t.Helper()
	raw, e := os.ReadFile("../../../../testdata/analytics.json")
	if e != nil {
		t.Fatal(e)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if e = protojson.Unmarshal(raw, req); e != nil {
		t.Fatal(e)
	}
	u, e := upsertOf(req)
	if e != nil {
		t.Fatal(e)
	}
	s := u.Snapshots[0]
	return domain.Analytics{AnalyticsData: domain.AnalyticsData{State: u.State, Snapshot: &s, Previous: ptr("2026-08"), Next: ptr("2026-10")}, Key: s.Key, Range: *s.Range, Current: true}
}
func composerForTest(t *testing.T) *Composer {
	t.Helper()
	c, e := NewComposer(prometheus.NewRegistry())
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func byID(resp *analyticsv1.GetAnalyticsResponse, id string) *commonv1.Block {
	for _, b := range resp.Blocks {
		if b.Id == id {
			return b
		}
	}
	return nil
}
func fixtureMetric(s *domain.Analytics, id string) *domain.Metric {
	for i := range s.Snapshot.Metrics {
		if s.Snapshot.Metrics[i].ID == id {
			return &s.Snapshot.Metrics[i]
		}
	}
	panic(id)
}
func setDetails(t *testing.T, s *domain.Analytics, id string, m *analyticsv1.Metric) {
	t.Helper()
	b, e := detailsMarshal.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	fixtureMetric(s, id).Details = b
}
func valueOf(b *commonv1.Block) map[string]any { return b.GetData().AsMap() }

func TestUnitComposerFullScreenKeepsFactsActionsAndTexts(t *testing.T) {
	s := screenFixture(t)
	got, e := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got.Currency != "RUB" || got.Params.PeriodKey != "2026-09" || !got.Period.IsCurrent || got.State.UnreadLinesCount != 5 || got.State.CalculatedAt.AsTime() != *s.Snapshot.CalculatedAt {
		t.Fatalf("header %+v", got)
	}
	ids := []string{}
	for _, b := range got.Blocks {
		ids = append(ids, b.Id)
		if b.Type != "heading" && (b.GetAltText() == "" || b.V != 1) {
			t.Fatalf("accessibility %+v", b)
		}
	}
	if !reflect.DeepEqual(ids, []string{"tiles", "monthly_chart", "expense_categories", "income_categories", "", "regular_payments", "notable_spending", "bank_fees", "small_frequent", "balance"}) {
		t.Fatal(ids)
	}
	tiles := valueOf(byID(got, "tiles"))["tiles"].([]any)
	second := tiles[1].(map[string]any)
	if second["id"] != "forecast" || second["value"] != float64(10120000) {
		t.Fatal(second)
	}
	income := tiles[3].(map[string]any)
	if income["show_plus"] != true {
		t.Fatal(income)
	}
	cat := valueOf(byID(got, "expense_categories"))
	if cat["visible_count"] != float64(4) || len(cat["items"].([]any)) != 2 {
		t.Fatal(cat)
	}
	refund := cat["items"].([]any)[1].(map[string]any)
	if refund["value"] != float64(-12345) {
		t.Fatal(refund)
	}
	if _, ok := refund["share"]; ok {
		t.Fatal("negative category invented a share")
	}
	regular := valueOf(byID(got, "regular_payments"))
	if byID(got, "regular_payments").Status != "tentative" || regular["badge"] != "предварительно" || len(regular["items"].([]any)) != 3 || regular["items_more"].(map[string]any)["value"] != float64(90000) {
		t.Fatal(regular)
	}
	button := regular["items"].([]any)[0].(map[string]any)["button"].(map[string]any)
	if button["type"] != "dismiss_regular_payment" || button["label"] != "Не подписка" {
		t.Fatal(button)
	}
	fees := byID(got, "bank_fees")
	if fees.Action.GetFilter().GetSelection() != "s_bank_fees" || fees.Action.GetFilter().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITH {
		t.Fatal(fees)
	}
	small := valueOf(byID(got, "small_frequent"))
	if !strings.HasPrefix(small["description"].(string), "по августу ·") {
		t.Fatal(small)
	}
	if a := actionJSON(byID(got, "small_frequent").Action.Filter, time.FixedZone("MSK", 10800))["filter"].(map[string]any); a["from"] != "2026-08-01T00:00:00+03:00" || a["selection"] != "s_small_coffee" {
		t.Fatal(a)
	}
	chart := valueOf(byID(got, "monthly_chart"))
	if chart["highlight_index"] != float64(1) || len(chart["notes"].([]any)) != 2 {
		t.Fatal(chart)
	}
	if point := chart["points"].([]any)[1].(map[string]any); point["data_to"] != "2026-09-25T21:40:00+03:00" || point["note"] != "по 25-е" {
		t.Fatal(point)
	}
	bal := valueOf(byID(got, "balance"))
	if bal["value_kind"] != "percent" || bal["summary"].(map[string]any)["value"] != float64(7258000) {
		t.Fatal(bal)
	}
}

func TestUnitComposerSupportsExperimentsAndPeriodVariants(t *testing.T) {
	for _, tc := range []struct {
		typ   domain.PeriodType
		exp   map[string]string
		first string
	}{{domain.PeriodMonth, map[string]string{"analytics_order": "b"}, "expense_categories"}, {domain.PeriodQuarter, nil, "tiles"}, {domain.PeriodYear, map[string]string{"analytics_order": "unknown"}, "tiles"}} {
		s := screenFixture(t)
		s.Key.Type = tc.typ
		s.Current = false
		got, e := composerForTest(t).Compose(t.Context(), s, nil, tc.exp)
		if e != nil {
			t.Fatal(e)
		}
		if got.Blocks[0].Id != tc.first {
			t.Fatal(got.Blocks[0])
		}
		if tc.typ != domain.PeriodMonth && byID(got, "bank_fees") != nil {
			t.Fatal("observations outside month")
		}
		if tc.typ == domain.PeriodYear && byID(got, "balance") != nil {
			t.Fatal("balance in year")
		}
		if tc.typ != domain.PeriodYear {
			tiles := valueOf(byID(got, "tiles"))["tiles"].([]any)
			if tiles[1].(map[string]any)["id"] != "expense" {
				t.Fatal(tiles)
			}
		}
	}
	s := screenFixture(t)
	c := composerForTest(t)
	got, e := c.Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "ranked_list", MaxVersion: 2}}, nil)
	if e != nil || len(got.Blocks) != 2 {
		t.Fatalf("supports %+v %v", got, e)
	}
	got, e = c.Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "chart", MaxVersion: 0}, {Type: "future", MaxVersion: 99}}, nil)
	if e != nil || len(got.Blocks) != 0 {
		t.Fatalf("unknown supports %+v %v", got, e)
	}
	chart := fixtureMetric(&s, "monthly_chart")
	chart.Status = 3
	chart.Lock = &domain.Lock{Reason: 2, Required: ptr(int32(2)), Available: ptr(int32(1))}
	got, e = c.Compose(t.Context(), s, nil, nil)
	if e != nil || byID(got, "monthly_chart") != nil {
		t.Fatalf("early %+v %v", got, e)
	}
	s.Stale = true
	got, e = c.Compose(t.Context(), s, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if valueOf(byID(got, "tiles"))["tiles"].([]any)[1].(map[string]any)["id"] != "expense" {
		t.Fatal("stale forecast")
	}
}

func TestUnitComposerMissingCorruptAndUnsafeMetricsDegrade(t *testing.T) {
	s := screenFixture(t)
	c := composerForTest(t)
	fixtureMetric(&s, "expense_categories").Details = []byte("{")
	got, e := c.Compose(t.Context(), s, nil, nil)
	if e != nil || byID(got, "expense_categories").Status != "unavailable" || byID(got, "bank_fees").Status != "ready" {
		t.Fatalf("partial %+v %v", got, e)
	}
	fixtureMetric(&s, "tiles.income").ValueMinor = ptr(int64(1 << 53))
	got, e = c.Compose(t.Context(), s, nil, nil)
	if e != nil || byID(got, "tiles").Status != "unavailable" {
		t.Fatalf("unsafe integer %+v %v", got, e)
	}
	s.Snapshot = nil
	got, e = c.Compose(t.Context(), s, nil, nil)
	if e != nil || got.Period.Bounds.Coverage != commonv1.Coverage_COVERAGE_NO_DATA || byID(got, "tiles").Status != "unavailable" {
		t.Fatalf("no snapshot %+v %v", got, e)
	}
	if _, e = c.Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "heading", MaxVersion: 1}}, nil); e != nil {
		t.Fatal(e)
	}
	s = screenFixture(t)
	for i := range s.Snapshot.Metrics {
		s.Snapshot.Metrics[i].Details = []byte("{")
	}
	if _, e = c.Compose(t.Context(), s, nil, nil); e == nil {
		t.Fatal("all blocks broken accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = c.Compose(ctx, s, nil, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	s.State.TimeZone = "No/Such_Zone"
	if _, e = c.Compose(t.Context(), s, nil, nil); e == nil {
		t.Fatal("invalid zone")
	}
}

func TestUnitComposerMetricStatesAndLockWindows(t *testing.T) {
	for _, reason := range []int32{0, 1, 2, 3, 4, 99} {
		for _, available := range []int32{0, 1} {
			s := screenFixture(t)
			for _, id := range []string{"insights.regular_payments", "insights.notable_spending", "insights.small_frequent", "tiles.forecast", "monthly_chart"} {
				m := fixtureMetric(&s, id)
				m.Status = 3
				m.Lock = &domain.Lock{Reason: reason, Required: ptr(int32(3)), Available: ptr(available), AvailableFrom: s.Range.Start}
			}
			got, e := composerForTest(t).Compose(t.Context(), s, nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			for _, id := range []string{"regular_payments", "notable_spending", "small_frequent"} {
				b := byID(got, id)
				if b.Status != "locked" || b.GetLock().GetText() == "" || len(valueOf(b)) != 2 {
					t.Fatalf("lock %+v", b)
				}
			}
		}
	}
	s := screenFixture(t)
	for i := range s.Snapshot.Metrics {
		s.Snapshot.Metrics[i].Status = 4
		s.Snapshot.Metrics[i].ValueMinor = nil
	}
	got, e := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"expense_categories", "income_categories", "regular_payments", "notable_spending", "bank_fees", "small_frequent"} {
		if b := byID(got, id); b.Status != "none" || b.GetEmptyText() == "" {
			t.Fatal(b)
		}
	}
	s = screenFixture(t)
	fixtureMetric(&s, "tiles.balance").Details = nil
	got, e = composerForTest(t).Compose(t.Context(), s, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(valueOf(byID(got, "balance"))["caption"].(string), "Доходов за период нет") {
		t.Fatal("zero-income caption")
	}
	s = screenFixture(t)
	setDetails(t, &s, "tiles.balance", &analyticsv1.Metric{Details: &analyticsv1.Metric_Balance{Balance: &analyticsv1.BalanceDetails{ShareOfIncome: ptr(-0.12)}}})
	got, e = composerForTest(t).Compose(t.Context(), s, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(valueOf(byID(got, "balance"))["caption"].(string), "расходы по выписке больше доходов") {
		t.Fatal("negative share")
	}
}

func TestUnitComposerGuardsAndPresentationBoundaries(t *testing.T) {
	if _, e := NewComposer(nil); e == nil {
		t.Fatal("nil registry")
	}
	reg := prometheus.NewRegistry()
	if _, e := NewComposer(reg); e != nil {
		t.Fatal(e)
	}
	if _, e := NewComposer(reg); e == nil {
		t.Fatal("duplicate registry")
	}
	for _, name := range []string{"finassist_analytics_blocks_total", "finassist_analytics_blocks_unavailable_total"} {
		r := prometheus.NewRegistry()
		r.MustRegister(prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "occupied"}, []string{"block_id"}))
		if _, e := NewComposer(r); e == nil {
			t.Fatal("occupied metric")
		}
	}
	_, e := guardBlock(t.Context(), func(ctx context.Context) ([]*commonv1.Block, error) { <-ctx.Done(); return nil, ctx.Err() })
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	for _, v := range []any{int64(1 << 53), int64(-1 << 53), math.NaN(), map[string]any{"x": []any{math.Inf(1)}}} {
		if safeNumbers(v) == nil {
			t.Fatal(v)
		}
	}
	if _, e := data(map[string]any{"value": int64(1<<53 - 1)}); e != nil {
		t.Fatal(e)
	}
	if money(math.MinInt64) != "−92 233 720 368 547 758,08 ₽" || money(1) != "0,01 ₽" || money(100) != "1 ₽" {
		t.Fatal("money representation")
	}
	for _, n := range []int64{1, 2, 5, 11, 14, 21} {
		want := map[int64]string{1: "one", 2: "few", 5: "many", 11: "many", 14: "many", 21: "one"}[n]
		if noun(n, "one", "few", "many") != want {
			t.Fatal(n)
		}
	}
	if actionJSON(&commonv1.OperationsFilter{TransferMode: 1, SelectionName: ptr("label")}, time.UTC) != nil {
		t.Fatal("empty action")
	}
	if actionJSON(&commonv1.OperationsFilter{Kind: 99}, time.UTC) != nil {
		t.Fatal("unknown kind")
	}
	if actionJSON(&commonv1.OperationsFilter{Q: ptr("q"), TransferMode: 99}, time.UTC) != nil {
		t.Fatal("unknown mode")
	}
	if a := actionJSON(&commonv1.OperationsFilter{Q: ptr("q"), AmountFromMinor: ptr(int64(-1)), AmountToMinor: ptr(int64(0)), TransferMode: 2}, time.UTC); a["filter"].(map[string]any)["transfer_mode"] != "without" {
		t.Fatal(a)
	}
	if actionJSON(&commonv1.OperationsFilter{Q: ptr(""), CategoryId: ptr(""), Selection: ptr("")}, time.UTC) != nil {
		t.Fatal("blank filter created an action")
	}
	if blockAction(nil, time.UTC) != nil {
		t.Fatal("nil action")
	}
	if dateString(nil, time.UTC) != nil {
		t.Fatal("nil date")
	}
	s := screenFixture(t)
	p := composition{s: s, zone: time.UTC}
	if p.lock("notable_spending", nil).Text == "" {
		t.Fatal("nil lock")
	}
	if p.lock("notable_spending", &analyticsv1.MetricLock{Reason: 1}).Text == "" {
		t.Fatal("no threshold")
	}
	if timerange(nil) != nil || timestamp(nil) != nil || filter(nil) != nil {
		t.Fatal("nil conversion")
	}
	m := domain.Metric{Status: 3, Range: &domain.TimeRange{Start: s.Range.Start, End: s.Range.End}, Lock: &domain.Lock{Reason: 1, Required: ptr(int32(3))}, Basis: &domain.Basis{FullMonths: 3, Range: &s.Range}, Filter: &domain.Filter{From: s.Range.Start, To: s.Range.End, Kind: 1, TransferMode: 1}}
	if got, e := metric(m); e != nil || got.Lock.Required == nil || got.Basis.FullMonths != 3 || got.Filters.Kind != 1 {
		t.Fatalf("conversion %+v %v", got, e)
	}
}

type fakeScreen struct {
	result domain.Analytics
	err    error
	user   string
	typ    domain.PeriodType
	key    *string
	mode   domain.TransferMode
}

func (f *fakeScreen) Get(_ context.Context, user string, typ domain.PeriodType, key *string, mode domain.TransferMode) (domain.Analytics, error) {
	f.user = user
	f.typ = typ
	f.key = key
	f.mode = mode
	return f.result, f.err
}
func TestUnitGetAnalyticsHandler(t *testing.T) {
	f := &fakeScreen{result: screenFixture(t)}
	h := &analytics{screen: f, composer: composerForTest(t)}
	ctx := reqctx.WithUserID(t.Context(), user)
	got, e := h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if e != nil || !got.HasData || f.user != user || f.typ != domain.PeriodMonth || f.mode != domain.TransferWith {
		t.Fatalf("default %+v %v", got, e)
	}
	for _, req := range []*analyticsv1.GetAnalyticsRequest{{Period: 99}, {TransferMode: 99}} {
		if _, e = h.GetAnalytics(ctx, req); status.Code(e) != codes.InvalidArgument {
			t.Fatal(e)
		}
	}
	if _, e = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{UserId: ptr("01a107db-ae80-7b07-b8a1-000000000002")}); !bareInvalid(e) {
		t.Fatal(e)
	}
	f.err = domain.ErrInvalid
	if _, e = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{}); status.Code(e) != codes.InvalidArgument {
		t.Fatal(e)
	}
	f.err = context.DeadlineExceeded
	if _, e = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	f.err = nil
	f.result = domain.Analytics{}
	got, e = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if e != nil || !proto.Equal(got, &analyticsv1.GetAnalyticsResponse{}) {
		t.Fatalf("no data %+v %v", got, e)
	}
	f.result.State = &domain.State{HasData: false}
	got, e = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if e != nil || got.HasData {
		t.Fatal(e)
	}
	if _, e = (&analytics{}).GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{}); e == nil {
		t.Fatal("missing wiring")
	}
	if _, e = h.GetAnalytics(t.Context(), &analyticsv1.GetAnalyticsRequest{UserId: ptr("bad")}); !bareInvalid(e) {
		t.Fatal(e)
	}
}

func TestUnitComposerCountersAndMalformedPresentationFacts(t *testing.T) {
	reg := prometheus.NewRegistry()
	c, err := NewComposer(reg)
	if err != nil {
		t.Fatal(err)
	}
	s := screenFixture(t)
	fixtureMetric(&s, "expense_categories").Details = []byte(`{"categories":"broken"}`)
	if _, err = c.Compose(t.Context(), s, nil, nil); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]float64{}
	for _, family := range families {
		for _, m := range family.Metric {
			if len(m.Label) != 1 || m.Label[0].GetName() != "block_id" {
				t.Fatal(m)
			}
			counts[family.GetName()+":"+m.Label[0].GetValue()] = m.GetCounter().GetValue()
		}
	}
	if counts["finassist_analytics_blocks_total:expense_categories"] != 1 || counts["finassist_analytics_blocks_unavailable_total:expense_categories"] != 1 || counts["finassist_analytics_blocks_unavailable_total:income_categories"] != 0 {
		t.Fatal(counts)
	}
	s = screenFixture(t)
	setDetails(t, &s, "tiles.balance", &analyticsv1.Metric{Details: &analyticsv1.Metric_Balance{Balance: &analyticsv1.BalanceDetails{ShareOfIncome: ptr(math.NaN())}}})
	if _, err = metric(*fixtureMetric(&s, "tiles.balance")); err == nil {
		t.Fatal("non-finite share accepted")
	}
	p := composition{s: s, zone: time.UTC, metrics: map[string]domain.Metric{}}
	if got := p.tile("income", "Доходы", &analyticsv1.Metric{Status: 1}, ""); got["status"] != "unavailable" {
		t.Fatal(got)
	}
	if _, err = data(map[string]any{"unknown": make(chan int)}); err == nil {
		t.Fatal("unsupported JSON value")
	}
	from, to := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if dayRange(from, to) != "31 декабря 2025 — 2 января 2026" || dayRange(from, from) != "31 декабря" {
		t.Fatal("cross-year day text")
	}
	if monthRange(&commonv1.TimeRange{Start: timestamp(&from), End: timestamp(&to)}, time.UTC) != "декабрь 2025 — январь 2026" {
		t.Fatal("cross-year month text")
	}
	if coverage(99) != "" || coverage(commonv1.Coverage_COVERAGE_NO_DATA) != "no_data" {
		t.Fatal("coverage fallback")
	}
	point := &analyticsv1.MonthlyPoint{Range: &commonv1.TimeRange{Start: timestamp(&from), End: timestamp(&to)}, Bounds: &commonv1.DataBounds{Coverage: 2, DataFrom: timestamp(&from), DataTo: timestamp(&to), Gaps: []*commonv1.TimeRange{{Start: timestamp(&from), End: timestamp(&to)}}}}
	if !strings.HasPrefix(p.barNote(point), "без ") {
		t.Fatal("gap note")
	}
}
