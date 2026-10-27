package app_test

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
	"github.com/fin-assist/analytics/internal/config"
)

func revisionList(values map[string]int64) []*common.DataRevision {
	out := make([]*common.DataRevision, 0, len(values))
	for code, value := range values {
		out = append(out, &common.DataRevision{Code: code, Value: value})
	}
	return out
}
func revisionsMap(t *testing.T, list []*common.DataRevision) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range list {
		if r == nil || r.Value < 0 {
			t.Fatal("invalid returned provenance", list)
		}
		if _, found := out[r.Code]; found {
			t.Fatal("duplicate returned code", list)
		}
		out[r.Code] = r.Value
	}
	return out
}
func revisionsWant(t *testing.T, list []*common.DataRevision, want map[string]int64) {
	t.Helper()
	if got := revisionsMap(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("provenance %+v want%+v", got, want)
	}
}
func versionedRequest(user string, version int32, key string, state, period map[string]int64) *pb.UpsertUserMetricsRequest {
	var periods []*pb.PeriodSnapshot
	if key != "" {
		periods = both(key, version, common.Coverage_COVERAGE_COMPLETE)
	}
	req := upsert(user, version, periods)
	req.State.DataRevisions = revisionList(state)
	for _, p := range req.Periods {
		p.DataRevisions = revisionList(period)
	}
	return req
}
func TestAPISourceProvenanceBelongsToSelectedSnapshot(t *testing.T)         { provenanceScenario(t) }
func TestIntegrationSourceProvenanceBelongsToSelectedSnapshot(t *testing.T) { provenanceScenario(t) }
func provenanceScenario(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	old := map[string]int64{"categories": 1, "uploads": 2, "time_zone": 1, "future_source": math.MaxInt64}
	current := map[string]int64{"categories": 2, "uploads": 2, "time_zone": 2, "future_source": math.MaxInt64}
	first := versionedRequest(alice, 1, "2026-09", old, old)
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), first); err != nil {
		t.Fatal(err)
	}
	get := func(key string) *pb.GetAnalyticsResponse {
		t.Helper()
		r, err := c.analytics.GetAnalytics(apitest.Caller(ctx, "assistant"), &pb.GetAnalyticsRequest{UserId: proto.String(alice), PeriodKey: proto.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	revisionsWant(t, get("2026-09").DataRevisions, old)
	// Repeating exactly the immutable producer payload preserves the same vector.
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), proto.Clone(first).(*pb.UpsertUserMetricsRequest)); err != nil {
		t.Fatal(err)
	}
	dirty := versionedRequest(alice, 2, "", current, nil)
	dirty.State.Recalculating = true
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), dirty); err != nil {
		t.Fatal(err)
	}
	selected := get("2026-09")
	if !selected.State.Recalculating {
		t.Fatal("fixture did not retain old snapshot")
	}
	revisionsWant(t, selected.DataRevisions, map[string]int64{"uploads": 2, "future_source": math.MaxInt64})
	revisionsWant(t, get("2026-08").DataRevisions, current)
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 3, "2026-08", current, current)); err != nil {
		t.Fatal(err)
	}
	list, err := c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &pb.ListPeriodsRequest{UserId: proto.String(alice)})
	if err != nil || len(list.Periods) != 2 {
		t.Fatal(list, err)
	}
	revisionsWant(t, list.DataRevisions, map[string]int64{"uploads": 2, "future_source": math.MaxInt64})
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 4, "2026-07", current, map[string]int64{"uploads": 2})); err != nil {
		t.Fatal(err)
	}
	list, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &pb.ListPeriodsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	revisionsWant(t, list.DataRevisions, map[string]int64{"uploads": 2})
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 5, "2026-06", current, nil)); err != nil {
		t.Fatal(err)
	}
	list, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &pb.ListPeriodsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	revisionsWant(t, list.DataRevisions, map[string]int64{})
	revisionsWant(t, get("2026-06").DataRevisions, map[string]int64{})
	// Fresh production assembly against the stored database proves provenance is
	// durable rather than reconstructed from process memory or the latest state.
	fresh := apitest.Start(t, func(cfg *config.Config) { cfg.DB.PrimaryDSN = env.DSN })
	restored, err := pb.NewAnalyticsServiceClient(fresh.Conn).GetAnalytics(apitest.User(ctx, alice), &pb.GetAnalyticsRequest{PeriodKey: proto.String("2026-09")})
	if err != nil {
		t.Fatal(err)
	}
	revisionsWant(t, restored.DataRevisions, map[string]int64{"uploads": 2, "future_source": math.MaxInt64})
	newest := map[string]int64{"categories": 8, "uploads": 4, "time_zone": 2, "future_source": math.MaxInt64}
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 8, "2026-09", newest, newest)); err != nil {
		t.Fatal(err)
	}
	stale, err := c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 7, "2026-09", current, current))
	if err != nil || stale.AppliedPeriods != 0 || stale.SkippedStalePeriods != 2 {
		t.Fatal(stale, err)
	}
	revisionsWant(t, get("2026-09").DataRevisions, newest)
	// Another tenant cannot obtain either the selected vector or the state vector.
	foreign, err := c.analytics.GetAnalytics(apitest.User(ctx, bob), &pb.GetAnalyticsRequest{})
	if err != nil || foreign.HasData || len(foreign.DataRevisions) != 0 {
		t.Fatal("tenant provenance leak", foreign, err)
	}
	empty := versionedRequest(alice, 9, "", newest, nil)
	empty.State.HasData = false
	for _, key := range []string{"2026-09", "2026-08", "2026-07", "2026-06"} {
		empty.RemovedPeriods = append(empty.RemovedPeriods, &pb.PeriodRef{Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: key})
	}
	if _, err = c.ingest.UpsertUserMetrics(producer(ctx), empty); err != nil {
		t.Fatal(err)
	}
	noData := get("2026-09")
	if noData.HasData || noData.State != nil || noData.Period != nil || noData.TimeZone != "" || noData.Currency != "" || len(noData.Blocks) != 0 {
		t.Fatal("no-data exposed financial fields", noData)
	}
	revisionsWant(t, noData.DataRevisions, newest)
	list, err = c.analytics.ListPeriods(apitest.User(ctx, alice), &pb.ListPeriodsRequest{})
	if err != nil || len(list.Periods) != 0 {
		t.Fatal(list, err)
	}
	revisionsWant(t, list.DataRevisions, newest)
	_, err = c.deletion.PurgeUserData(apitest.Caller(ctx, "user"), &deletion.PurgeUserDataRequest{UserId: alice})
	if err != nil {
		t.Fatal(err)
	}
	late, err := c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 10, "2026-09", newest, newest))
	if err != nil || late.AppliedPeriods != 0 || rowCount(t, env, alice) != 0 {
		t.Fatal("late provenance resurrected user", late, err)
	}
}

func TestAPISourceRevisionsRejectInvalidWholeBatch(t *testing.T) { invalidProvenanceScenario(t) }
func TestIntegrationSourceRevisionsRejectInvalidWholeBatch(t *testing.T) {
	invalidProvenanceScenario(t)
}
func invalidProvenanceScenario(t *testing.T) {
	env, c := startAPI(t)
	ctx := t.Context()
	initial := map[string]int64{"categories": 1, "uploads": 1, "time_zone": 0}
	if _, err := c.ingest.UpsertUserMetrics(producer(ctx), versionedRequest(alice, 1, "2026-09", initial, initial)); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]*common.DataRevision{
		"duplicate": {{Code: "categories", Value: 1}, {Code: "categories", Value: 2}}, "negative": {{Code: "categories", Value: -1}}, "uppercase": {{Code: "Categories", Value: 1}}, "empty": {{Code: "", Value: 1}}, "nil": {nil},
	}
	many := []*common.DataRevision{}
	for i := range 33 {
		many = append(many, &common.DataRevision{Code: fmt.Sprintf("source_%d", i), Value: 0})
	}
	cases["too_many"] = many
	for name, bad := range cases {
		for _, field := range []string{"state", "period"} {
			t.Run(name+"/"+field, func(t *testing.T) {
				req := versionedRequest(alice, 2, "2026-08", map[string]int64{"categories": 2}, map[string]int64{"categories": 2})
				if field == "state" {
					req.State.DataRevisions = bad
				} else {
					req.Periods[1].DataRevisions = bad
				}
				_, err := c.ingest.UpsertUserMetrics(producer(ctx), req)
				if status.Code(err) != codes.InvalidArgument || len(status.Convert(err).Details()) != 0 {
					t.Fatal("producer refusal must be bare INVALID_ARGUMENT", err)
				}
				list, err := c.analytics.ListPeriods(apitest.User(ctx, alice), &pb.ListPeriodsRequest{})
				if err != nil || len(list.Periods) != 1 {
					t.Fatal("invalid batch partially applied", list, err)
				}
				revisionsWant(t, list.DataRevisions, initial)
			})
		}
	}
	if rowCount(t, env, bob) != 0 {
		t.Fatal("foreign tenant changed")
	}
}

func TestAPIResponseProvenanceIncludesStateAndSnapshot(t *testing.T) { responseProvenanceScenario(t) }
func TestIntegrationResponseProvenanceIncludesStateAndSnapshot(t *testing.T) {
	responseProvenanceScenario(t)
}
func responseProvenanceScenario(t *testing.T) {
	for _, order := range []string{"period-then-state", "state-then-period", "legacy-state"} {
		t.Run(order, func(t *testing.T) {
			_, c := startAPI(t)
			state := map[string]int64{"categories": 1, "uploads": 4, "time_zone": 1, "future_source": math.MaxInt64}
			period := map[string]int64{"categories": 1, "uploads": 5, "time_zone": 2, "future_source": math.MaxInt64}
			initial := versionedRequest(alice, 1, "2026-09", period, period)
			if order != "period-then-state" {
				initial.State.DataRevisions = revisionList(state)
			}
			if order == "legacy-state" {
				initial.State.DataRevisions = nil
			}
			if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), initial); err != nil {
				t.Fatal(err)
			}
			if order == "period-then-state" {
				update := versionedRequest(alice, 2, "", state, nil)
				update.State.Recalculating = true
				if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), update); err != nil {
					t.Fatal(err)
				}
			} else {
				update := versionedRequest(alice, 2, "2026-09", nil, period)
				update.State = nil // A period-only update cannot replace the stored screen state.
				if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), update); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string]int64{"categories": 1, "future_source": math.MaxInt64}
			if order == "legacy-state" {
				want = map[string]int64{}
			}
			response, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "assistant"), &pb.GetAnalyticsRequest{UserId: proto.String(alice), PeriodKey: proto.String("2026-09")})
			if err != nil || !response.HasData || response.State == nil {
				t.Fatal(response, err)
			}
			revisionsWant(t, response.DataRevisions, want)
			list, err := c.analytics.ListPeriods(apitest.Caller(t.Context(), "assistant"), &pb.ListPeriodsRequest{UserId: proto.String(alice)})
			if err != nil || len(list.Periods) != 1 {
				t.Fatal(list, err)
			}
			revisionsWant(t, list.DataRevisions, want)
		})
	}
}

func TestAPIStoredRevisionNullIsNotKnownZero(t *testing.T)         { corruptRevisionScenario(t) }
func TestIntegrationStoredRevisionNullIsNotKnownZero(t *testing.T) { corruptRevisionScenario(t) }
func corruptRevisionScenario(t *testing.T) {
	env, c := startAPI(t)
	known := map[string]int64{"categories": 0, "uploads": 0, "time_zone": 0}
	for _, user := range []string{alice, bob} {
		if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), versionedRequest(user, 1, "2026-09", known, known)); err != nil {
			t.Fatal(err)
		}
	}
	for _, storage := range []struct{ table, update string }{
		{"user_data_state", `UPDATE user_data_state SET data_revisions=$2 WHERE user_id=$1`},
		{"period_snapshot", `UPDATE period_snapshot SET data_revisions=$2 WHERE user_id=$1`},
	} {
		for _, bad := range []string{`{"uploads":null}`, `{"uploads":true}`, `{"uploads":0.0}`, `[]`} {
			if _, err := env.App.DB.Primary().Exec(t.Context(), storage.update, alice, bad); err != nil {
				t.Fatal(err)
			}
			for _, read := range []func() error{
				func() error {
					_, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "assistant"), &pb.GetAnalyticsRequest{UserId: proto.String(alice), PeriodKey: proto.String("2026-09")})
					return err
				},
				func() error {
					_, err := c.analytics.ListPeriods(apitest.User(t.Context(), alice), &pb.ListPeriodsRequest{})
					return err
				},
			} {
				if err := read(); status.Code(err) != codes.Internal {
					t.Fatal("corrupt source claimed known provenance", storage.table, bad, err)
				}
			}
			foreign, err := c.analytics.GetAnalytics(apitest.User(t.Context(), bob), &pb.GetAnalyticsRequest{PeriodKey: proto.String("2026-09")})
			if err != nil {
				t.Fatal("corrupt tenant affected another user", err)
			}
			revisionsWant(t, foreign.DataRevisions, known)
		}
		if _, err := env.App.DB.Primary().Exec(t.Context(), storage.update, alice, `{"categories":0,"uploads":0,"time_zone":0}`); err != nil {
			t.Fatal(err)
		}
	}
}
