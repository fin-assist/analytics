package app_test

// API tests of MetricsIngestService.UpsertUserMetrics, AnalyticsService.ListPeriods and
// UserDeletionService.PurgeUserData (FIN-283) through the whole assembly: interceptors, the
// access table of grpc-api.md 3.3, the tombstone admission, handlers and PostgreSQL with every
// migration source. Calls are made as bank-statement-data, bank-bff, assistant and user would.

import (
	"context"
	"strings"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

const (
	alice = "01a107db-ae80-7b07-b8a1-00000000a11c"
	bob   = "01a107db-ae80-7b07-b8a1-000000000b0b"
)

var msk = time.FixedZone("MSK", 3*3600)

func at(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func monthStart(key string) time.Time {
	start, err := time.ParseInLocation("2006-01", key, msk)
	if err != nil {
		panic(err)
	}
	return start
}

func snapshot(key string, mode commonv1.TransferMode, version int32, coverage commonv1.Coverage) *analyticsv1.PeriodSnapshot {
	start := monthStart(key)
	return &analyticsv1.PeriodSnapshot{
		Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: key, TransferMode: mode,
		Range:            &commonv1.TimeRange{Start: at(start), End: at(start.AddDate(0, 1, 0))},
		Bounds:           &commonv1.DataBounds{Coverage: coverage, DataFrom: at(start), DataTo: at(start.Add(20 * time.Hour))},
		UnreadLinesCount: version, CalculatedAt: at(time.Date(2026, 9, 25, 21, 52, 0, 0, msk)),
		Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "tiles.expense", DefinitionVersion: 1, Metric: &analyticsv1.Metric{
			Status: analyticsv1.MetricStatus_METRIC_STATUS_READY, ValueMinor: proto.Int64(int64(version)),
			Lock: &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS, Text: "ignored producer text"},
		}}},
	}
}

func both(key string, version int32, coverage commonv1.Coverage) []*analyticsv1.PeriodSnapshot {
	return []*analyticsv1.PeriodSnapshot{
		snapshot(key, commonv1.TransferMode_TRANSFER_MODE_WITH, version, coverage),
		snapshot(key, commonv1.TransferMode_TRANSFER_MODE_WITHOUT, version, coverage),
	}
}

func dataState() *analyticsv1.UserDataState {
	from, to := monthStart("2026-08"), time.Date(2026, 9, 26, 0, 0, 0, 0, msk)
	return &analyticsv1.UserDataState{HasData: true, DataFrom: at(from), DataTo: at(to), LastOperationAt: at(to.Add(-time.Hour)),
		StaleAfter: at(to.AddDate(0, 0, 14)), FullMonths: 1, TimeZone: "Europe/Moscow", CalculatedAt: at(to)}
}

func upsert(user string, version int32, periods []*analyticsv1.PeriodSnapshot, removed ...string) *analyticsv1.UpsertUserMetricsRequest {
	r := &analyticsv1.UpsertUserMetricsRequest{UserId: user, CalcVersion: version, State: dataState(), Periods: periods}
	for _, key := range removed {
		r.RemovedPeriods = append(r.RemovedPeriods, &analyticsv1.PeriodRef{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: key})
	}
	return r
}

type clients struct {
	ingest    analyticsv1.MetricsIngestServiceClient
	analytics analyticsv1.AnalyticsServiceClient
	deletion  userdeletionv1.UserDeletionServiceClient
}

func startAPI(t *testing.T) (*apitest.Env, clients) {
	t.Helper()
	env := apitest.Start(t)
	return env, clients{
		ingest:    analyticsv1.NewMetricsIngestServiceClient(env.Conn),
		analytics: analyticsv1.NewAnalyticsServiceClient(env.Conn),
		deletion:  userdeletionv1.NewUserDeletionServiceClient(env.Conn),
	}
}

func producer(ctx context.Context) context.Context { return apitest.Caller(ctx, "bank-statement-data") }

// rowCount is the number of rows the user has in the service's tables.
func rowCount(t *testing.T, env *apitest.Env, user string) int {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	var n int
	if err := conn.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM user_data_state WHERE user_id = $1) + (SELECT count(*) FROM period_snapshot WHERE user_id = $1) +
		(SELECT count(*) FROM period_gap WHERE user_id = $1) + (SELECT count(*) FROM period_metric WHERE user_id = $1)`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func keys(t *testing.T, resp *analyticsv1.ListPeriodsResponse) []string {
	t.Helper()
	var out []string
	for _, p := range resp.GetPeriods() {
		out = append(out, p.GetKey())
	}
	return out
}

func wantBare(t *testing.T, err error, code codes.Code) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code || apitest.Reason(err) != "" || len(st.Details()) != 0 {
		t.Fatalf("error %v, want a bare %s", err, code)
	}
}

func TestAPIUpsertUserMetricsAppliesByVersion(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()

	resp, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 5,
		append(both("2026-09", 5, commonv1.Coverage_COVERAGE_PARTIAL), both("2026-08", 5, commonv1.Coverage_COVERAGE_COMPLETE)...), "2026-07"))
	if err != nil || resp.GetAppliedPeriods() != 4 || resp.GetSkippedStalePeriods() != 0 {
		t.Fatalf("first batch: %v, %v", resp, err)
	}
	// Outbox redelivery of the same version: applied again, nothing changes.
	resp, err = c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 5, both("2026-09", 5, commonv1.Coverage_COVERAGE_PARTIAL)))
	if err != nil || resp.GetAppliedPeriods() != 2 || resp.GetSkippedStalePeriods() != 0 {
		t.Fatalf("redelivery: %v, %v", resp, err)
	}
	// An older version is acknowledged and skipped (skipped_stale_periods), including a
	// snapshot of the removed period: the removal marker keeps it removed.
	resp, err = c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 4,
		append(both("2026-09", 4, commonv1.Coverage_COVERAGE_COMPLETE), both("2026-07", 4, commonv1.Coverage_COVERAGE_PARTIAL)...)))
	if err != nil || resp.GetAppliedPeriods() != 0 || resp.GetSkippedStalePeriods() != 4 {
		t.Fatalf("stale batch: %v, %v", resp, err)
	}

	list, err := c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(t, list); len(got) != 2 || got[0] != "2026-09" || got[1] != "2026-08" || list.GetTimeZone() != "Europe/Moscow" {
		t.Fatalf("periods %v in %q, want 2026-09, 2026-08 in Europe/Moscow", got, list.GetTimeZone())
	}
	if p := list.GetPeriods()[0]; p.GetCoverage() != commonv1.Coverage_COVERAGE_PARTIAL ||
		!p.GetRange().GetStart().AsTime().Equal(monthStart("2026-09")) || !p.GetRange().GetEnd().AsTime().Equal(monthStart("2026-10")) {
		t.Fatalf("2026-09 is %v, want the v5 partial snapshot with its calendar range", p)
	}
	if list.GetPeriods()[1].GetCoverage() != commonv1.Coverage_COVERAGE_COMPLETE {
		t.Fatalf("2026-08 coverage %v", list.GetPeriods()[1].GetCoverage())
	}

	// A newer batch removes a period from both modes.
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 6, nil, "2026-08")); err != nil {
		t.Fatal(err)
	}
	list, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH})
	if err != nil || len(list.GetPeriods()) != 1 || list.GetPeriods()[0].GetKey() != "2026-09" {
		t.Fatalf("after removal: %v, %v", keys(t, list), err)
	}
	// Another user's data is untouched by all of this.
	if n := rowCount(t, env, bob); n != 0 {
		t.Fatalf("bob has %d rows", n)
	}
}

// A batch that breaks a rule is refused whole with a bare INVALID_ARGUMENT (grpc-api.md 7.3):
// nothing of it is written, the outbox row stays.
func TestAPIUpsertUserMetricsRefusesInvalidBatch(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	cases := map[string]func(*analyticsv1.UpsertUserMetricsRequest){
		"status UNAVAILABLE from the producer": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[1].Metrics[0].Metric.Status = analyticsv1.MetricStatus_METRIC_STATUS_UNAVAILABLE
		},
		"period both sent and removed": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.RemovedPeriods = append(r.RemovedPeriods, &analyticsv1.PeriodRef{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09"})
		},
		"calc_version zero":  func(r *analyticsv1.UpsertUserMetricsRequest) { r.CalcVersion = 0 },
		"user_id not a UUID": func(r *analyticsv1.UpsertUserMetricsRequest) { r.UserId = "alice" },
		"period key 2026-9":  func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].PeriodKey = "2026-9" },
		"time zone Local":    func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.TimeZone = "Local" },
		"filter with q": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics[0].Metric.Filters = &commonv1.OperationsFilter{From: r.Periods[0].Range.Start, To: r.Periods[0].Range.End, Q: proto.String("кофе")}
		},
		"snapshot without range": func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Range = nil },
		"unknown transfer mode":  func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].TransferMode = commonv1.TransferMode(7) },
		"unknown period type":    func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Period = commonv1.PeriodType(7) },
		"same snapshot twice":    func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods = append(r.Periods, r.Periods[0]) },
		"removed twice": func(r *analyticsv1.UpsertUserMetricsRequest) {
			ref := &analyticsv1.PeriodRef{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-07"}
			r.RemovedPeriods = append(r.RemovedPeriods, ref, ref)
		},
		"removed with a bad key": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.RemovedPeriods = append(r.RemovedPeriods, &analyticsv1.PeriodRef{Period: commonv1.PeriodType_PERIOD_TYPE_QUARTER, PeriodKey: "2026-q3"})
		},
		"range reversed": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Range.Start, r.Periods[0].Range.End = r.Periods[0].Range.End, r.Periods[0].Range.Start
		},
		"coverage no data": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Bounds.Coverage = commonv1.Coverage_COVERAGE_NO_DATA
		},
		"no data bounds": func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Bounds.DataFrom = nil },
		"gap without a bound": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Bounds.Gaps = []*commonv1.TimeRange{{Start: r.Periods[0].Range.Start}}
		},
		"metric without id": func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].MetricId = "" },
		"metric twice": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics = append(r.Periods[0].Metrics, r.Periods[0].Metrics[0])
		},
		"metric without value":   func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].Metric = nil },
		"status unspecified":     func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].Metric.Status = 0 },
		"status beyond smallint": func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].Metric.Status = 65537 },
		"lock without reason":    func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[0].Metrics[0].Metric.Lock.Reason = 0 },
		"metric range half": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics[0].Metric.Range = &commonv1.TimeRange{Start: r.Periods[0].Range.Start}
		},
		"basis range reversed": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics[0].Metric.Basis = &analyticsv1.MetricBasis{Range: &commonv1.TimeRange{Start: r.Periods[0].Range.End, End: r.Periods[0].Range.Start}}
		},
		"filter without its period": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics[0].Metric.Filters = &commonv1.OperationsFilter{From: r.Periods[0].Range.Start}
		},
		"filter with an amount": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].Metrics[0].Metric.Filters = &commonv1.OperationsFilter{From: r.Periods[0].Range.Start, To: r.Periods[0].Range.End, AmountFromMinor: proto.Int64(1)}
		},
		"state without calculated_at": func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.CalculatedAt = nil },
		"state with one bound":        func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.DataTo = nil },
		"state stale before data_to":  func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.StaleAfter = r.State.DataFrom },
		"state data without ops":      func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.LastOperationAt = nil },
		"state negative full months":  func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.FullMonths = -1 },
		"invalid timestamp": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.Periods[0].CalculatedAt = &timestamppb.Timestamp{Nanos: -1}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := upsert(alice, 3, both("2026-09", 3, commonv1.Coverage_COVERAGE_PARTIAL))
			change(r)
			_, err := c.ingest.UpsertUserMetrics(producer(ctx), r)
			wantBare(t, err, codes.InvalidArgument)
			if n := rowCount(t, env, alice); n != 0 {
				t.Fatalf("the refused batch wrote %d rows", n)
			}
		})
	}
}

// grpc-api.md 3.3: MetricsIngestService only for bank-statement-data, as a system call.
func TestAPIUpsertUserMetricsRefusesForeignCallers(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	for name, call := range map[string]context.Context{
		"bank-bff with a user token": apitest.User(ctx, alice),
		"assistant":                  apitest.Caller(ctx, "assistant"),
		"user":                       apitest.Caller(ctx, "user"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.ingest.UpsertUserMetrics(call, upsert(alice, 1, both("2026-09", 1, commonv1.Coverage_COVERAGE_PARTIAL)))
			wantBare(t, err, codes.PermissionDenied)
		})
	}
	_, err := c.ingest.UpsertUserMetrics(ctx, upsert(alice, 1, both("2026-09", 1, commonv1.Coverage_COVERAGE_PARTIAL)))
	wantBare(t, err, codes.Unauthenticated)
	if n := rowCount(t, env, alice); n != 0 {
		t.Fatalf("a refused caller wrote %d rows", n)
	}
}

// PurgeUserData from user deletes the user's data and leaves a tombstone: a late batch is
// acknowledged without effects, the user's token is revoked, other users keep their data.
func TestAPIPurgeUserDataDeletesAndRemembers(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	for _, u := range []string{alice, bob} {
		if _, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(u, 1, both("2026-09", 1, commonv1.Coverage_COVERAGE_PARTIAL))); err != nil {
			t.Fatal(err)
		}
	}
	// Only user may delete.
	for _, caller := range []string{"bank-statement-data", "assistant"} {
		_, err := c.deletion.PurgeUserData(apitest.Caller(ctx, caller), &userdeletionv1.PurgeUserDataRequest{UserId: alice})
		wantBare(t, err, codes.PermissionDenied)
	}
	_, err := c.deletion.PurgeUserData(apitest.User(ctx, alice), &userdeletionv1.PurgeUserDataRequest{UserId: alice})
	wantBare(t, err, codes.PermissionDenied)
	if n := rowCount(t, env, alice); n == 0 {
		t.Fatal("a refused deletion removed data")
	}

	for range 2 { // the repeat is OK
		if _, err := c.deletion.PurgeUserData(apitest.Caller(ctx, "user"), &userdeletionv1.PurgeUserDataRequest{UserId: alice}); err != nil {
			t.Fatal(err)
		}
	}
	if n := rowCount(t, env, alice); n != 0 {
		t.Fatalf("alice has %d rows after PurgeUserData", n)
	}
	if n := rowCount(t, env, bob); n == 0 {
		t.Fatal("PurgeUserData of alice removed bob's data")
	}

	// A late batch of the deleted user: OK, nothing applied, nothing written.
	resp, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 2, both("2026-09", 2, commonv1.Coverage_COVERAGE_PARTIAL)))
	if err != nil || resp.GetAppliedPeriods() != 0 || resp.GetSkippedStalePeriods() != 0 {
		t.Fatalf("late batch: %v, %v", resp, err)
	}
	if n := rowCount(t, env, alice); n != 0 {
		t.Fatalf("a late batch brought back %d rows", n)
	}
	// The same user in upper case is the same deleted user.
	resp, err = c.ingest.UpsertUserMetrics(producer(ctx), upsert(strings.ToUpper(alice), 3, both("2026-09", 3, commonv1.Coverage_COVERAGE_PARTIAL)))
	if err != nil || resp.GetAppliedPeriods() != 0 || rowCount(t, env, alice) != 0 {
		t.Fatalf("late batch in upper case: %v, %v", resp, err)
	}
	// Any call with the deleted user's token: TOKEN_REVOKED.
	_, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{})
	if status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted user's call: %v, want UNAUTHENTICATED TOKEN_REVOKED", err)
	}
	_, err = c.analytics.GetAnalytics(apitest.User(ctx, alice), &analyticsv1.GetAnalyticsRequest{})
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted user's GetAnalytics: %v", err)
	}
	// An invalid user id is a sanitized refusal of the library, nothing is deleted.
	_, err = c.deletion.PurgeUserData(apitest.Caller(ctx, "user"), &userdeletionv1.PurgeUserDataRequest{UserId: "bob"})
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatalf("invalid user id: %v", err)
	}
}

func TestAPIListPeriodsModelsAndCallers(t *testing.T) {
	_, c := startAPI(t)
	ctx := t.Context()
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(alice, 1, both("2026-09", 1, commonv1.Coverage_COVERAGE_PARTIAL))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), upsert(bob, 1, both("2026-05", 1, commonv1.Coverage_COVERAGE_PARTIAL))); err != nil {
		t.Fatal(err)
	}

	// The user call reads the user of the token; repeating its own sub is allowed.
	for _, req := range []*analyticsv1.ListPeriodsRequest{{}, {UserId: proto.String(alice)}} {
		resp, err := c.analytics.ListPeriods(apitest.User(ctx, alice), req)
		if err != nil || len(resp.GetPeriods()) != 1 || resp.GetPeriods()[0].GetKey() != "2026-09" {
			t.Fatalf("alice's periods %v, %v", keys(t, resp), err)
		}
	}
	// Another user's id in a user call never selects that user: bare INVALID_ARGUMENT, nothing of bob's.
	_, err := c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{UserId: proto.String(bob)})
	wantBare(t, err, codes.InvalidArgument)

	// The system call of assistant names the user.
	resp, err := c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &analyticsv1.ListPeriodsRequest{UserId: proto.String(bob)})
	if err != nil || len(resp.GetPeriods()) != 1 || resp.GetPeriods()[0].GetKey() != "2026-05" {
		t.Fatalf("assistant's call for bob: %v, %v", keys(t, resp), err)
	}
	resp, err = c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &analyticsv1.ListPeriodsRequest{UserId: proto.String(strings.ToUpper(bob))})
	if err != nil || len(resp.GetPeriods()) != 1 {
		t.Fatalf("assistant's call for bob in upper case: %v, %v", keys(t, resp), err)
	}
	if _, err := c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{UserId: proto.String(strings.ToUpper(alice))}); err != nil {
		t.Fatalf("own sub in upper case: %v", err)
	}
	_, err = c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &analyticsv1.ListPeriodsRequest{})
	wantBare(t, err, codes.InvalidArgument)

	// A user without data: an empty list. Quarters of alice: none sent.
	resp, err = c.analytics.ListPeriods(apitest.User(ctx, "01a107db-ae80-7b07-b8a1-000000000eee"), &analyticsv1.ListPeriodsRequest{})
	if err != nil || len(resp.GetPeriods()) != 0 || resp.GetTimeZone() != "" {
		t.Fatalf("user without data: %v, %v", resp, err)
	}
	resp, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType_PERIOD_TYPE_QUARTER})
	if err != nil || len(resp.GetPeriods()) != 0 || resp.GetTimeZone() != "Europe/Moscow" {
		t.Fatalf("alice's quarters: %v, %v", resp, err)
	}
	// An unknown period type is a validation error of the field.
	_, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType(9)})
	if status.Code(err) != codes.InvalidArgument || apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("unknown period type: %v", err)
	}

	// Callers outside the table (grpc-api.md 3.3) and bank-bff without a user token.
	for _, caller := range []string{"bank-statement-data", "user"} {
		_, err := c.analytics.ListPeriods(apitest.Caller(ctx, caller), &analyticsv1.ListPeriodsRequest{UserId: proto.String(alice)})
		wantBare(t, err, codes.PermissionDenied)
	}
	_, err = c.analytics.ListPeriods(apitest.Caller(ctx, "bank-bff"), &analyticsv1.ListPeriodsRequest{UserId: proto.String(alice)})
	if status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("bank-bff without a user token: %v, want INVALID_TOKEN, not a system call", err)
	}
	bad := metadata.AppendToOutgoingContext(ctx, "x-service-authorization", "Bearer dev:bank-bff", "authorization", "Basic x")
	_, err = c.analytics.ListPeriods(bad, &analyticsv1.ListPeriodsRequest{UserId: proto.String(alice)})
	if apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("malformed authorization: %v", err)
	}
	// GetAnalytics reads the persisted screen through the same mixed-model access policy.
	_, err = c.analytics.GetAnalytics(apitest.User(ctx, alice), &analyticsv1.GetAnalyticsRequest{})
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}
}
