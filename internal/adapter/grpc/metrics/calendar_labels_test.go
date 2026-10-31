package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/domain"
)

func calendarFixture(t *testing.T, key, oldZone, newZone string) domain.Analytics {
	t.Helper()
	raw, err := os.ReadFile("../../../../testdata/analytics.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, r); err != nil {
		t.Fatal(err)
	}
	selected, err := time.Parse("2006-01", key)
	if err != nil {
		t.Fatal(err)
	}
	months := (selected.Year()-2026)*12 + int(selected.Month()) - 9
	old, err := time.LoadLocation(oldZone)
	if err != nil {
		t.Fatal(err)
	}
	msk, _ := time.LoadLocation("Europe/Moscow")
	var shift func(protoreflect.Message)
	shift = func(m protoreflect.Message) {
		if m.Descriptor().FullName() == "google.protobuf.Timestamp" {
			ts := m.Interface().(*timestamppb.Timestamp)
			civil := ts.AsTime().In(msk)
			moved := time.Date(civil.Year(), civil.Month(), civil.Day(), civil.Hour(), civil.Minute(), civil.Second(), civil.Nanosecond(), old).AddDate(0, months, 0)
			fresh := timestamppb.New(moved)
			ts.Seconds = fresh.Seconds
			ts.Nanos = fresh.Nanos
			return
		}
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.IsList() {
				if f.Kind() == protoreflect.MessageKind {
					list := v.List()
					for i := range list.Len() {
						shift(list.Get(i).Message())
					}
				}
				return true
			}
			if f.Kind() == protoreflect.MessageKind {
				shift(v.Message())
				return true
			}
			if f.Kind() == protoreflect.StringKind && (f.Name() == "month" || f.Name() == "period_key") {
				parsed, e := time.Parse("2006-01", v.String())
				if e != nil {
					t.Fatal(e)
				}
				m.Set(f, protoreflect.ValueOfString(parsed.AddDate(0, months, 0).Format("2006-01")))
			}
			return true
		})
	}
	shift(r.ProtoReflect())
	// The new state has rebuilt profile-day bounds while the financial snapshot retains its old calendar.
	profile, err := time.LoadLocation(newZone)
	if err != nil {
		t.Fatal(err)
	}
	civilEnd := r.State.DataTo.AsTime().In(old)
	boundary, ok := domain.CivilDayStart(civilEnd.Year(), civilEnd.Month(), civilEnd.Day(), profile)
	if !ok {
		t.Fatal("fixture day absent")
	}
	r.State.DataTo = timestamppb.New(boundary)
	r.State.TimeZone = newZone
	r.State.Recalculating = true
	u, err := upsertOf(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Validate(); err != nil {
		t.Fatal(err)
	}
	s := u.Snapshots[0]
	return domain.Analytics{AnalyticsData: domain.AnalyticsData{State: u.State, Snapshot: &s}, Key: s.Key, Range: *s.Range, Current: true}
}

// Compare every server-produced string, excluding serialized instants whose offsets
// legitimately follow the current transport zone while retaining the same moments.
func screenLabels(r *analyticsv1.GetAnalyticsResponse) map[string]string {
	labels := map[string]string{}
	var visit func(string, any)
	visit = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if _, err := time.Parse(time.RFC3339Nano, x); err != nil {
				labels[path] = x
			}
		case map[string]any:
			for key, v := range x {
				visit(path+"."+key, v)
			}
		case []any:
			for i, v := range x {
				visit(fmt.Sprintf("%s[%d]", path, i), v)
			}
		}
	}
	for i, b := range r.Blocks {
		prefix := fmt.Sprintf("%d:%s", i, b.Id)
		labels[prefix+".title"] = b.GetTitle()
		labels[prefix+".alt"] = b.GetAltText()
		labels[prefix+".empty"] = b.GetEmptyText()
		labels[prefix+".lock"] = b.GetLock().GetText()
		if b.Data != nil {
			visit(prefix, b.Data.AsMap())
		}
	}
	return labels
}

func TestUnitSnapshotLabelsStayInTheirCalendarForBothTimezoneDirections(t *testing.T) {
	for _, tc := range []struct{ key, old, new string }{{"2026-09", "Europe/Moscow", "UTC"}, {"2026-09", "UTC", "Europe/Moscow"}, {"2026-01", "Europe/Moscow", "UTC"}, {"2026-01", "UTC", "Europe/Moscow"}, {"2026-01", "America/New_York", "Asia/Tokyo"}, {"2026-01", "Asia/Tokyo", "America/New_York"}, {"2026-12", "Europe/Berlin", "UTC"}, {"2026-05", "Europe/Berlin", "UTC"}} {
		t.Run(tc.key+tc.old+tc.new, func(t *testing.T) {
			s := calendarFixture(t, tc.key, tc.old, tc.new)
			before, _ := json.Marshal(s.Snapshot)
			baseline := s
			state := *s.State
			state.TimeZone = tc.old
			baseline.State = &state
			c := composerForTest(t)
			want, err := c.Compose(t.Context(), baseline, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Compose(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(screenLabels(got), screenLabels(want)) {
				for key, w := range screenLabels(want) {
					if g := screenLabels(got)[key]; g != w {
						t.Errorf("%s: %q want %q", key, g, w)
					}
				}
			}
			if !proto.Equal(got.Period.Range, want.Period.Range) {
				t.Fatal("financial period boundaries changed")
			}
			after, _ := json.Marshal(s.Snapshot)
			if string(before) != string(after) {
				t.Fatal("stored snapshot mutated")
			}
			if tc.key == "2026-01" && !strings.HasPrefix(valueOf(byID(got, "small_frequent"))["description"].(string), "по декабрю 2025 ·") {
				t.Fatal("basis year boundary shifted")
			}
		})
	}
}

func TestUnitPeakAndAltMonthComeFromPointKey(t *testing.T) {
	for _, zones := range [][2]string{{"Europe/Moscow", "UTC"}, {"UTC", "Europe/Moscow"}} {
		s := calendarFixture(t, "2026-09", zones[0], zones[1])
		s.Key.Type = domain.PeriodQuarter
		s.Key.Key = "2026-Q3"
		old, _ := time.LoadLocation(zones[0])
		start, end := time.Date(2026, 7, 1, 0, 0, 0, 0, old), time.Date(2026, 10, 1, 0, 0, 0, 0, old)
		s.Range = domain.TimeRange{Start: &start, End: &end}
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		chart := byID(got, "monthly_chart")
		if !strings.Contains(chart.GetAltText(), "сентябрь 84 320 ₽") {
			t.Fatal(chart.GetAltText())
		}
		if !strings.HasPrefix(valueOf(chart)["notes"].([]any)[0].(map[string]any)["text"].(string), "Больше всего — в сентябре") {
			t.Fatal(valueOf(chart))
		}
	}
}

func TestUnitSmallFrequentInvalidGroupFilterKeepsMetricAction(t *testing.T) {
	for _, groupFilter := range []*commonv1.OperationsFilter{nil, {Kind: 99}, {TransferMode: 99, Selection: ptr("s_invalid")}} {
		s := screenFixture(t)
		m := fixtureMetric(&s, "insights.small_frequent")
		m.Filter = &domain.Filter{From: s.Range.Start, To: s.Range.End, TransferMode: 1, Selection: ptr("s_metric"), SelectionName: ptr("Мелкие частые траты")}
		original, err := metric(*m)
		if err != nil {
			t.Fatal(err)
		}
		original.GetSmallFrequent().Items[0].Filters = groupFilter
		setDetails(t, &s, m.ID, &analyticsv1.Metric{Details: original.Details})
		got, err := composerForTest(t).Compose(t.Context(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if byID(got, "small_frequent").Action.GetFilter().GetSelection() != "s_metric" {
			t.Fatal("valid metric action erased")
		}
	}
}
