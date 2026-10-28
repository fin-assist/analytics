package app_test

import (
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

func TestAPISourceProvenanceRequiresFinalCompletionProof(t *testing.T) {
	completionProofScenario(t)
}

func completionProofScenario(t *testing.T) {
	_, c := startAPI(t)
	ctx := t.Context()
	known := map[string]int64{"categories": 1, "uploads": 2, "time_zone": 0}
	publish := func(req *pb.UpsertUserMetricsRequest) {
		t.Helper()
		if _, err := c.ingest.UpsertUserMetrics(producer(ctx), req); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want map[string]int64, periods int, hasData bool) {
		t.Helper()
		// Both an old/new header and a missing selected header use screen state;
		// missing-period navigation cannot bypass the global completion proof.
		for _, key := range []string{"2026-09", "2026-08", "2026-07"} {
			for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
				r, err := c.analytics.GetAnalytics(apitest.Caller(ctx, "assistant"), &pb.GetAnalyticsRequest{UserId: proto.String(alice), PeriodKey: proto.String(key), TransferMode: mode})
				if err != nil || r.HasData != hasData {
					t.Fatal(key, mode, r, err)
				}
				revisionsWant(t, r.DataRevisions, want)
			}
		}
		list, err := c.analytics.ListPeriods(apitest.Caller(ctx, "assistant"), &pb.ListPeriodsRequest{UserId: proto.String(alice)})
		if err != nil || len(list.Periods) != periods {
			t.Fatal(list, err)
		}
		revisionsWant(t, list.DataRevisions, want)
	}
	first := versionedRequest(alice, 1, "2026-09", known, known)
	publish(first)
	check(known, 1, true)

	// A daily rebuild can add periods with every source counter unchanged. The
	// producer clears state provenance and awaits this ACK before sending parts.
	clearing := versionedRequest(alice, 2, "", nil, nil)
	clearing.State.Recalculating = false // No user action is being reported.
	publish(clearing)
	check(map[string]int64{}, 1, true)
	// A delayed old final/part cannot restore the proof after clearing committed.
	publish(proto.Clone(first).(*pb.UpsertUserMetricsRequest))
	check(map[string]int64{}, 1, true)

	// Known per-period vectors alone are insufficient: ListPeriods must stay
	// unknown even though every header it currently sees has the same vector.
	part := versionedRequest(alice, 3, "2026-08", nil, known)
	part.State.Recalculating = false
	publish(part)
	check(map[string]int64{}, 2, true)
	// Only the producer's final state, sent after all part/removal ACKs, confirms
	// the complete inventory, including missing-period/navigation responses.
	publish(versionedRequest(alice, 4, "", known, nil))
	check(known, 2, true)

	// Deleting the last periods also has a clearing/removal/final sequence. A
	// no-data response and an empty list must not invent proof before final ACK.
	removed := versionedRequest(alice, 5, "", nil, nil)
	removed.State.HasData = false
	for _, key := range []string{"2026-09", "2026-08"} {
		removed.RemovedPeriods = append(removed.RemovedPeriods, &pb.PeriodRef{Period: common.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: key})
	}
	publish(removed)
	check(map[string]int64{}, 0, false)
	final := versionedRequest(alice, 6, "", known, nil)
	final.State.HasData = false
	publish(final)
	check(known, 0, false)
}
