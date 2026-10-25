package metrics

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/fin-assist/go-grpc/server"

	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

const user = "01a107db-ae80-7b07-b8a1-000000000001"

type fakeUsecase struct {
	upserts []domain.Upsert
	result  usecase.Result
	err     error
	listed  []string
	periods domain.Periods
}

func (f *fakeUsecase) Upsert(_ context.Context, u domain.Upsert) (usecase.Result, error) {
	f.upserts = append(f.upserts, u)
	return f.result, f.err
}

func (f *fakeUsecase) Periods(_ context.Context, id string, typ domain.PeriodType) (domain.Periods, error) {
	f.listed = append(f.listed, id+" "+typ.String())
	return f.periods, f.err
}

func ts(day int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC))
}

func rng(from, to int) *commonv1.TimeRange { return &commonv1.TimeRange{Start: ts(from), End: ts(to)} }

func request() *analyticsv1.UpsertUserMetricsRequest {
	return &analyticsv1.UpsertUserMetricsRequest{
		UserId: user, CalcVersion: 4,
		State: &analyticsv1.UserDataState{HasData: true, DataFrom: ts(1), DataTo: ts(26), LastOperationAt: ts(25),
			StaleAfter: ts(28), FullMonths: 2, Recalculating: true, TimeZone: "Europe/Moscow", CalculatedAt: ts(26)},
		Periods: []*analyticsv1.PeriodSnapshot{{
			Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09", TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITHOUT,
			Range:            rng(1, 30),
			Bounds:           &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: ts(1), DataTo: ts(25), Gaps: []*commonv1.TimeRange{rng(3, 4)}},
			UnreadLinesCount: 2, CalculatedAt: ts(26),
			Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "tiles.forecast", DefinitionVersion: 3, Metric: &analyticsv1.Metric{
				Status: analyticsv1.MetricStatus_METRIC_STATUS_LOCKED, ValueMinor: proto.Int64(0), Range: rng(1, 2),
				Lock:    &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH, Required: proto.Int32(3), Available: proto.Int32(1), AvailableFrom: ts(10), Text: "producer text"},
				Basis:   &analyticsv1.MetricBasis{Range: rng(1, 5), FullMonths: 1},
				Filters: &commonv1.OperationsFilter{From: ts(1), To: ts(30), CategoryId: proto.String("cat"), Kind: commonv1.KindFilter_KIND_FILTER_INCOME, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH, Selection: proto.String("sel"), SelectionName: proto.String("name")},
				Details: &analyticsv1.Metric_BankFees{BankFees: &analyticsv1.BankFees{}},
			}}},
		}},
		RemovedPeriods: []*analyticsv1.PeriodRef{{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, PeriodKey: "2025"}},
	}
}

func TestUnitUpsertOfMapsEveryField(t *testing.T) {
	u, err := upsertOf(request())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Validate(); err != nil {
		t.Fatalf("converted request is invalid: %v", err)
	}
	if u.UserID != user || u.Version != 4 || len(u.Removed) != 1 || u.Removed[0] != (domain.PeriodKey{Type: domain.PeriodYear, Key: "2025"}) {
		t.Fatalf("batch %+v", u)
	}
	st := u.State
	if !st.HasData || !st.Recalculating || st.FullMonths != 2 || st.TimeZone != "Europe/Moscow" || !st.StaleAfter.Equal(ts(28).AsTime()) || !st.LastOperationAt.Equal(ts(25).AsTime()) {
		t.Fatalf("state %+v", st)
	}
	s := u.Snapshots[0]
	if s.Key != (domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodMonth, Key: "2026-09"}, Mode: domain.TransferWithout}) ||
		s.Coverage != domain.CoveragePartial || s.UnreadLines != 2 || len(s.Gaps) != 1 || !s.Gaps[0].End.Equal(ts(4).AsTime()) {
		t.Fatalf("snapshot %+v", s)
	}
	m := s.Metrics[0]
	if m.ID != "tiles.forecast" || m.DefinitionVersion != 3 || m.Status != 3 || *m.ValueMinor != 0 || !m.Range.Start.Equal(ts(1).AsTime()) {
		t.Fatalf("metric %+v", m)
	}
	if m.Lock.Reason != int32(analyticsv1.LockReason_LOCK_REASON_TOO_EARLY_IN_MONTH) || *m.Lock.Required != 3 || *m.Lock.Available != 1 || !m.Lock.AvailableFrom.Equal(ts(10).AsTime()) {
		t.Fatalf("lock %+v", m.Lock)
	}
	if m.Basis.FullMonths != 1 || !m.Basis.Range.End.Equal(ts(5).AsTime()) {
		t.Fatalf("basis %+v", m.Basis)
	}
	f := m.Filter
	if *f.CategoryID != "cat" || f.Kind != 2 || f.TransferMode != 1 || *f.Selection != "sel" || *f.SelectionName != "name" || !f.To.Equal(ts(30).AsTime()) {
		t.Fatalf("filter %+v", f)
	}
	if string(m.Details) != `{"bank_fees":{}}` {
		t.Fatalf("details %s, want the variant key with an empty message", m.Details)
	}
	// Without a state, metrics or details nothing is invented.
	bare, err := upsertOf(&analyticsv1.UpsertUserMetricsRequest{UserId: user, CalcVersion: 1,
		Periods: []*analyticsv1.PeriodSnapshot{{Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "x", Metric: &analyticsv1.Metric{}}}}}})
	if err != nil || bare.State != nil || bare.Snapshots[0].Range != nil || bare.Snapshots[0].Metrics[0].Details != nil || bare.Snapshots[0].Metrics[0].Lock != nil {
		t.Fatalf("bare batch %+v, %v", bare, err)
	}
}

// What the domain cannot represent is refused by the conversion, never stored differently.
func TestUnitUpsertOfRefusesWhatCannotBeStored(t *testing.T) {
	metric := func(r *analyticsv1.UpsertUserMetricsRequest) *analyticsv1.Metric {
		return r.Periods[0].Metrics[0].Metric
	}
	cases := map[string]func(r *analyticsv1.UpsertUserMetricsRequest){
		"metric without value": func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].Metric = nil },
		"filter q":             func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.Q = proto.String("кофе") },
		"filter amount from":   func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.AmountFromMinor = proto.Int64(1) },
		"filter amount to":     func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.AmountToMinor = proto.Int64(0) },
		"invalid state moment": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.State.CalculatedAt = &timestamppb.Timestamp{Nanos: -1}
		},
		"invalid range": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Range.End = &timestamppb.Timestamp{Seconds: 1 << 62}
		},
		"invalid data_from": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Bounds.DataFrom = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid data_to": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Bounds.DataTo = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid calculated": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].CalculatedAt = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid gap": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Bounds.Gaps[0].Start = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid metric range": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Range.Start = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid lock moment": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Lock.AvailableFrom = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid basis": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Basis.Range.End = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid filter from": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Filters.From = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid filter to": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Filters.To = &timestamppb.Timestamp{Nanos: 2e9}
		},
		"invalid range start": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Range.Start = &timestamppb.Timestamp{Nanos: 2e9}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := request()
			change(r)
			if _, err := upsertOf(r); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error %v, want ErrInvalid", err)
			}
		})
	}
	// A nil gap of a hand-built message becomes a gap without bounds, which validation refuses.
	r := request()
	r.Periods[0].Bounds.Gaps = []*commonv1.TimeRange{nil}
	u, err := upsertOf(r)
	if err != nil || !errors.Is(u.Validate(), domain.ErrInvalid) {
		t.Fatalf("nil gap: %v, %v", err, u.Validate())
	}
}

func TestUnitUpsertUserMetricsHandler(t *testing.T) {
	uc := &fakeUsecase{result: usecase.Result{Applied: 3, Skipped: 1}}
	h := &ingest{uc: uc, log: slog.New(slog.DiscardHandler)}
	resp, err := h.UpsertUserMetrics(t.Context(), request())
	if err != nil || resp.GetAppliedPeriods() != 3 || resp.GetSkippedStalePeriods() != 1 || len(uc.upserts) != 1 {
		t.Fatalf("response %v, %v", resp, err)
	}

	// A conversion refusal and a validation refusal are both a bare INVALID_ARGUMENT.
	r := request()
	r.Periods[0].Metrics[0].Metric.Filters.Q = proto.String("x")
	if _, err := h.UpsertUserMetrics(t.Context(), r); !bareInvalid(err) || len(uc.upserts) != 1 {
		t.Fatalf("conversion refusal: %v, usecase called %d times", err, len(uc.upserts))
	}
	uc.err = errors.Join(domain.ErrInvalid, errors.New("status UNAVAILABLE"))
	if _, err := h.UpsertUserMetrics(t.Context(), request()); !bareInvalid(err) {
		t.Fatalf("validation refusal: %v", err)
	}
	// Any other error goes to the interceptor unchanged; it answers INTERNAL without its text.
	boom := errors.New("connection reset")
	uc.err = boom
	if _, err := h.UpsertUserMetrics(t.Context(), request()); !errors.Is(err, boom) {
		t.Fatalf("other error: %v", err)
	}
}

func bareInvalid(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.InvalidArgument && len(st.Details()) == 0 && server.Reason(err) == "" &&
		st.Message() == codes.InvalidArgument.String()
}

func TestUnitListPeriods(t *testing.T) {
	start, end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	uc := &fakeUsecase{periods: domain.Periods{TimeZone: "Europe/Moscow", Items: []domain.PeriodItem{{Key: "2026-09", Start: start, End: end, Coverage: domain.CoverageComplete}}}}
	h := &analytics{uc: uc}
	asUser := reqctx.WithUserID(t.Context(), user)

	resp, err := h.ListPeriods(asUser, &analyticsv1.ListPeriodsRequest{})
	if err != nil || resp.GetTimeZone() != "Europe/Moscow" || len(resp.GetPeriods()) != 1 {
		t.Fatalf("response %v, %v", resp, err)
	}
	p := resp.GetPeriods()[0]
	if p.GetKey() != "2026-09" || p.GetCoverage() != commonv1.Coverage_COVERAGE_COMPLETE || !p.GetRange().GetStart().AsTime().Equal(start) || !p.GetRange().GetEnd().AsTime().Equal(end) {
		t.Fatalf("period %v", p)
	}
	// UNSPECIFIED is month; a user call may repeat its own sub.
	if _, err := h.ListPeriods(asUser, &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_YEAR, UserId: proto.String(user)}); err != nil {
		t.Fatal(err)
	}
	// A system call names the user.
	if _, err := h.ListPeriods(t.Context(), &analyticsv1.ListPeriodsRequest{UserId: proto.String(user), Period: commonv1.PeriodType_PERIOD_TYPE_QUARTER}); err != nil {
		t.Fatal(err)
	}
	if want := []string{user + " month", user + " year", user + " quarter"}; strings.Join(uc.listed, ",") != strings.Join(want, ",") {
		t.Fatalf("listed %v, want %v", uc.listed, want)
	}

	// Another user's id in a user call, a system call without user_id: bare INVALID_ARGUMENT,
	// the usecase does not run.
	other := "01a107db-ae80-7b07-b8a1-000000000002"
	for name, call := range map[string]func() error{
		"another user in a user call": func() error {
			_, err := h.ListPeriods(asUser, &analyticsv1.ListPeriodsRequest{UserId: proto.String(other)})
			return err
		},
		"system call without user_id": func() error {
			_, err := h.ListPeriods(t.Context(), &analyticsv1.ListPeriodsRequest{})
			return err
		},
		"system call with empty user_id": func() error {
			_, err := h.ListPeriods(t.Context(), &analyticsv1.ListPeriodsRequest{UserId: proto.String("")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(uc.listed)
			if err := call(); !bareInvalid(err) || len(uc.listed) != before {
				t.Fatalf("error %v, usecase calls %d → %d", err, before, len(uc.listed))
			}
		})
	}

	// An unknown period type is a validation error of the field.
	if _, err := h.ListPeriods(asUser, &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType(9)}); server.Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("unknown period type: %v", err)
	}
	uc.err = domain.ErrInvalid
	if _, err := h.ListPeriods(t.Context(), &analyticsv1.ListPeriodsRequest{UserId: proto.String("not-a-uuid")}); !bareInvalid(err) {
		t.Fatalf("invalid user id: %v", err)
	}
	boom := errors.New("replica down")
	uc.err = boom
	if _, err := h.ListPeriods(asUser, &analyticsv1.ListPeriodsRequest{}); !errors.Is(err, boom) {
		t.Fatalf("store error: %v", err)
	}
}

func TestUnitRegister(t *testing.T) {
	s := grpc.NewServer()
	Register(s, &fakeUsecase{}, slog.New(slog.DiscardHandler))
	info := s.GetServiceInfo()
	if _, ok := info["finassist.analytics.v1.MetricsIngestService"]; !ok {
		t.Fatal("MetricsIngestService not registered")
	}
	if _, ok := info["finassist.analytics.v1.AnalyticsService"]; !ok {
		t.Fatal("AnalyticsService not registered")
	}
	// GetAnalytics is FIN-5.
	if _, err := (&analytics{}).GetAnalytics(t.Context(), &analyticsv1.GetAnalyticsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetAnalytics: %v", err)
	}
}

func TestUnitSnapshotOfIsTheHandlerConversion(t *testing.T) {
	r := request()
	got, err := SnapshotOf(r.Periods[0])
	if err != nil {
		t.Fatal(err)
	}
	u, _ := upsertOf(r)
	if got.Key != u.Snapshots[0].Key || len(got.Metrics) != 1 {
		t.Fatalf("snapshot %+v", got)
	}
}

func TestUnitRefusedRuleIsBoundedInTheLog(t *testing.T) {
	if got := bounded("short"); got != "short" {
		t.Fatal(got)
	}
	long := strings.Repeat("ж", maxRuleLog)
	got := bounded(long)
	if len(got) > maxRuleLog+len("…") || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("bounded %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

func TestUnitUserIDIsCanonicalizedAtTheBoundary(t *testing.T) {
	upper := strings.ToUpper(user)
	for in, want := range map[string]string{
		upper: user, user: user, "01a107db-AE80-7b07-b8a1-000000000001": user,
		// Not the canonical grammar: left as is for validation to refuse.
		"{" + user + "}": "{" + user + "}", strings.ReplaceAll(user, "-", ""): strings.ReplaceAll(user, "-", ""), "u1": "u1", "": "",
	} {
		if got := canonicalUserID(in); got != want {
			t.Errorf("canonicalUserID(%q) = %q, want %q", in, got, want)
		}
	}
	r := request()
	r.UserId = upper
	u, err := upsertOf(r)
	if err != nil || u.UserID != user {
		t.Fatalf("batch user %q, %v; want %q", u.UserID, err, user)
	}
	// ListPeriods: a request user_id in another case is the same user; the sub is canonical too.
	asUpper := reqctx.WithUserID(t.Context(), upper)
	for _, requested := range []*string{nil, proto.String(""), proto.String(user), proto.String(upper)} {
		if got, ok := subject(asUpper, requested); !ok || got != user {
			t.Fatalf("subject(%v) = %q, %v; want %q", requested, got, ok, user)
		}
	}
	if got, ok := subject(t.Context(), proto.String(upper)); !ok || got != user {
		t.Fatalf("system subject %q, %v", got, ok)
	}
	if _, ok := subject(asUpper, proto.String("01a107db-ae80-7b07-b8a1-000000000002")); ok {
		t.Fatal("another user accepted")
	}
}
