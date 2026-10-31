package metrics

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

func allTimeFixture(t *testing.T, mode int) domain.Analytics {
	t.Helper()
	raw, err := os.ReadFile("../../../../testdata/all_time.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	u, err := upsertOf(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Validate(); err != nil {
		t.Fatal(err)
	}
	s := u.Snapshots[mode-1]
	return domain.Analytics{AnalyticsData: domain.AnalyticsData{State: u.State, Snapshot: &s}, Key: s.Key, Range: *s.Range}
}

func TestUnitGroupedTilesPreserveEveryFactAndActionAcrossPeriodsAndModes(t *testing.T) {
	for _, typ := range []domain.PeriodType{domain.PeriodMonth, domain.PeriodQuarter, domain.PeriodYear, domain.PeriodAllTime} {
		for mode := 1; mode <= 2; mode++ {
			s := screenFixture(t)
			s.Key.Type = typ
			if typ == domain.PeriodQuarter {
				s.Key.Key = "2026-Q3"
				s.Current = false
			}
			if typ == domain.PeriodYear {
				s.Key.Key = "2026"
				s.Current = false
			}
			if typ == domain.PeriodAllTime {
				s = allTimeFixture(t, mode)
			}
			for _, stale := range []bool{false, true} {
				s.Stale = stale
				c := composerForTest(t)
				legacy, err := c.Compose(t.Context(), s, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				one, err := c.Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 1}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(valueOf(byID(legacy, "tiles")), valueOf(byID(one, "tiles"))) {
					t.Fatal("v1 fallback changed")
				}
				two, err := c.Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}, {Type: "tile_group", MaxVersion: 1}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				b := byID(two, "tiles")
				if b.V != 2 || b.Status != byID(legacy, "tiles").Status {
					t.Fatal(b)
				}
				groups := valueOf(b)["groups"].([]any)
				if len(groups) != 2 {
					t.Fatal(groups)
				}
				flat := append(groups[0].(map[string]any)["tiles"].([]any), groups[1].(map[string]any)["tiles"].([]any)...)
				old := map[string]map[string]any{}
				for _, v := range valueOf(byID(legacy, "tiles"))["tiles"].([]any) {
					m := v.(map[string]any)
					old[m["id"].(string)] = m
				}
				ids := []string{}
				for _, v := range flat {
					m := v.(map[string]any)
					id := m["id"].(string)
					ids = append(ids, id)
					previous := old[id]
					for _, field := range []string{"value", "status", "value_kind", "approx", "action", "show_plus", "lock"} {
						if !reflect.DeepEqual(previous[field], m[field]) {
							t.Fatalf("%v mode%d %s %s changed", typ, mode, id, field)
						}
					}
					if id == "daily_expense" && (m["status"] == "ready" || m["status"] == "tentative") && !strings.HasPrefix(m["caption"].(string), "По дням с данными не позже сегодня; пропуски не учитываются") {
						t.Fatal(m)
					}
				}
				if !reflect.DeepEqual(ids[:4], []string{"expense", "income", "balance", "daily_expense"}) {
					t.Fatal(ids)
				}
			}
		}
	}
}

func TestUnitAllTimeKeepsGapCoverageZeroMonthActionsAndYearLabels(t *testing.T) {
	for mode := 1; mode <= 2; mode++ {
		s := allTimeFixture(t, mode)
		s.State.FullMonths = 0
		// Calendar-only payloads cannot leak into all_time, even in retained rows.
		s.Snapshot.Metrics = append(s.Snapshot.Metrics, domain.Metric{ID: "tiles.forecast", Details: []byte(`{broken`)}, domain.Metric{ID: "tiles.daily_expense.comparison", Details: []byte(`{broken`)})
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Params.PeriodKey != "all_time" || got.Period.IsCurrent || got.Navigation.Previous != nil || got.Navigation.Next != nil || got.Period.Bounds.Coverage != 2 || len(got.Period.Bounds.Gaps) != 1 {
			t.Fatal(got)
		}
		if len(got.Blocks) != 4 || byID(got, "balance") != nil || byID(got, "regular_payments") != nil {
			t.Fatal(got.Blocks)
		}
		d := valueOf(byID(got, "monthly_chart"))
		points := d["points"].([]any)
		if len(points) != 11 || d["highlight_index"] != nil || !strings.Contains(byID(got, "monthly_chart").GetAltText(), "декабрь 2025") {
			t.Fatal(d)
		}
		for i, raw := range points {
			p := raw.(map[string]any)
			a, ok := p["action"].(map[string]any)
			if i == 0 || i == 1 || i == 10 {
				if !ok || a["type"] != "select_analytics_period" || a["period"] != "month" || len(a["date"].(string)) != 7 {
					t.Fatal(p)
				}
			} else if ok || p["value"] != nil {
				t.Fatal(p)
			}
		}
		if points[1].(map[string]any)["value"] != float64(0) {
			t.Fatal("covered zero disappeared")
		}
		if strings.Contains(byID(got, "monthly_chart").GetAltText(), "типичный") {
			t.Fatal("all_time typical year guessed")
		}
	}
	s := screenFixture(t)
	got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range valueOf(byID(got, "balance"))["points"].([]any) {
		if p.(map[string]any)["action"] != nil {
			t.Fatal("balance gained month-selection action")
		}
	}
}

func TestUnitAllTimeBeforeDeliveryRetainsBoundsWithoutInventingZeroFacts(t *testing.T) {
	s := allTimeFixture(t, 1)
	s.Snapshot = nil
	for _, version := range []int32{1, 2} {
		got, err := composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: version}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		b := byID(got, "tiles")
		if b.V != version || b.Status != "unavailable" || got.Period.Bounds.Coverage != 2 || got.Period.Bounds.DataFrom == nil || got.Period.Bounds.DataTo == nil {
			t.Fatal(got)
		}
		if version == 2 && len(valueOf(b)["groups"].([]any)) != 0 {
			t.Fatal(b)
		}
	}
	zone, _ := time.LoadLocation("Europe/Moscow")
	p := composition{s: s, zone: zone}
	if !p.preciseAllTimeDays() {
		t.Fatal("exact current bounds lost")
	}
	p.s.State.Recalculating = true
	if p.preciseAllTimeDays() {
		t.Fatal("recalculating certified")
	}
	p.s.State.Recalculating = false
	shifted := s.Range.Start.Add(time.Hour)
	p.s.Range.Start = &shifted
	if p.preciseAllTimeDays() {
		t.Fatal("mismatched range certified")
	}
	p.s.State = nil
	if p.preciseAllTimeDays() {
		t.Fatal("absent state certified")
	}
}

func TestUnitSupportsRejectMalformedVersionsBeforeStorage(t *testing.T) {
	for _, supports := range [][]*commonv1.BlockSupport{{nil}, {{Type: "", MaxVersion: 1}}, {{Type: "tile_group", MaxVersion: 0}}, {{Type: "future", MaxVersion: -1}}} {
		store := &validationStore{}
		screen, err := usecase.NewScreen(store, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		h := &analytics{screen: screen, composer: composerForTest(t)}
		_, err = h.GetAnalytics(reqctx.WithUserID(t.Context(), user), &analyticsv1.GetAnalyticsRequest{Supports: supports})
		rest, ok := finerr.ToREST(err)
		if !ok || len(rest.Violations) != 1 || rest.Violations[0].Field != "supports" || rest.Violations[0].Reason != "INVALID_FORMAT" || store.reads != 0 {
			t.Fatalf("%+v %v reads%d", rest, err, store.reads)
		}
	}
	for _, s := range [][]*commonv1.BlockSupport{nil, {}, {{Type: "tile_group", MaxVersion: 1}}, {{Type: "tile_group", MaxVersion: 2}, {Type: "tile_group", MaxVersion: 1}}, {{Type: "future", MaxVersion: 2147483647}}} {
		want := int32(1)
		if len(s) == 2 {
			want = 2
		}
		if len(s) == 1 && s[0].Type == "future" {
			want = 0
		}
		if supportedVersion("tile_group", s) != want {
			t.Fatal(s)
		}
	}
}

func TestUnitGroupedTilesKeepSignedZerosLocksAndEmptyGroups(t *testing.T) {
	for _, amount := range []int64{0, -123456, 9007199254740991} {
		s := screenFixture(t)
		fixtureMetric(&s, "tiles.expense").ValueMinor = &amount
		got, err := composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		groups := valueOf(byID(got, "tiles"))["groups"].([]any)
		if groups[0].(map[string]any)["tiles"].([]any)[0].(map[string]any)["value"] != float64(amount) {
			t.Fatal(groups)
		}
	}
	s := screenFixture(t)
	m := fixtureMetric(&s, "tiles.daily_expense")
	m.Status = 3
	m.ValueMinor = nil
	m.Lock = &domain.Lock{Reason: 2, Required: ptr(int32(2)), Available: ptr(int32(1))}
	got, err := composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	daily := valueOf(byID(got, "tiles"))["groups"].([]any)[1].(map[string]any)["tiles"].([]any)[0].(map[string]any)
	if daily["status"] != "locked" || daily["value"] != nil || daily["caption"] != nil || daily["lock"] == nil {
		t.Fatal(daily)
	}
	s.Snapshot = nil
	got, err = composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := byID(got, "tiles"); b.Status != "none" || b.V != 2 || len(valueOf(b)["groups"].([]any)) != 0 {
		t.Fatal(b)
	}
	// A malformed internal tile payload degrades this block rather than fabricating facts.
	p := composition{tileVersion: 2}
	for _, payload := range []map[string]any{{}, {"tiles": []any{1}}, {"tiles": []any{map[string]any{}}}, {"tiles": []any{map[string]any{"id": "expense"}}}} {
		d, err := data(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.groupTiles(&commonv1.Block{Status: "ready", Data: d}); err == nil {
			t.Fatal(payload)
		}
	}
}

func TestUnitAllTimeTimezoneRebuildRetainsFactsWithoutInventingDayPrecision(t *testing.T) {
	s := allTimeFixture(t, 1)
	s.State.TimeZone = "America/New_York"
	s.State.Recalculating = true
	before := *s.Range.Start
	got, err := composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expense := valueOf(byID(got, "tiles"))["groups"].([]any)[0].(map[string]any)["tiles"].([]any)[0].(map[string]any)
	if !strings.Contains(expense["caption"].(string), "За всё время") || !strings.Contains(expense["caption"].(string), "идёт пересчёт") || !got.Period.Range.Start.AsTime().Equal(before) || expense["value"] != float64(35000) {
		t.Fatal(expense)
	}
	s.State.Recalculating = false
	zone, _ := time.LoadLocation(s.State.TimeZone)
	p := composition{s: s, zone: zone}
	if p.preciseAllTimeDays() {
		t.Fatal("old Moscow midnights certified in a new zone")
	}
	if p.chartMonthName("invalid", monthNom) != monthName("invalid", monthNom) {
		t.Fatal("invalid month guessed year")
	}
}

func TestUnitMonthSelectionRequiresCoveredCanonicalMonthIncludingZero(t *testing.T) {
	for _, tc := range []struct {
		key      string
		coverage commonv1.Coverage
		want     bool
	}{
		{"2025-12", 1, true}, {"2026-01", 2, true}, {"2026-01", 3, false}, {"2026-01", 0, false}, {"2026-01", 99, false}, {"2026", 1, false}, {"2026-1", 2, false}, {"2026-13", 1, false},
	} {
		point := &analyticsv1.MonthlyPoint{Month: tc.key, ExpenseMinor: ptr(int64(0)), Bounds: &commonv1.DataBounds{Coverage: tc.coverage}}
		action := monthSelection(point)
		if (action != nil) != tc.want {
			t.Fatal(tc, action)
		}
		if action != nil && (action.Type != "select_analytics_period" || action.GetPeriod() != commonv1.PeriodType_PERIOD_TYPE_MONTH || action.GetPeriodKey() != tc.key) {
			t.Fatal(action)
		}
	}
	if monthSelection(nil) != nil {
		t.Fatal("missing point became interactive")
	}
}

func TestUnitAllTimeIgnoresCalendarOnlyRowsWithoutLosingAnyFact(t *testing.T) {
	for _, version := range []int32{1, 2} {
		for mode := 1; mode <= 2; mode++ {
			s := allTimeFixture(t, mode)
			s.Snapshot.Metrics = append(s.Snapshot.Metrics, domain.Metric{ID: "tiles.forecast", Details: []byte(`{broken`)}, domain.Metric{ID: "tiles.daily_expense.comparison", Details: []byte(`{broken`)})
			response, err := composerForTest(t).Compose(t.Context(), s, []*commonv1.BlockSupport{{Type: "tile_group", MaxVersion: version}, {Type: "chart", MaxVersion: 1}, {Type: "ranked_list", MaxVersion: 1}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			b := byID(response, "tiles")
			if b == nil || b.Status != "ready" || b.V != version {
				t.Fatalf("calendar-only row degraded all_time v%d mode%d: %v", version, mode, b)
			}
			var flat []any
			if version == 1 {
				flat = valueOf(b)["tiles"].([]any)
			} else {
				for _, raw := range valueOf(b)["groups"].([]any) {
					flat = append(flat, raw.(map[string]any)["tiles"].([]any)...)
				}
			}
			want := map[string]float64{"expense": 35000, "income": 50000, "balance": 15000, "daily_expense": 555}
			if mode == 2 {
				want["expense"] = 30000
				want["balance"] = 20000
				want["daily_expense"] = 476
			}
			if len(flat) != len(want) {
				t.Fatal(flat)
			}
			for _, raw := range flat {
				tile := raw.(map[string]any)
				id := tile["id"].(string)
				expected, ok := want[id]
				if !ok || tile["status"] != "ready" || tile["value"] != expected {
					t.Fatalf("v%d mode%d fact %s: %v", version, mode, id, tile)
				}
			}
		}
	}
}
