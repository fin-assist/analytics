package app_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

// Exact write statements of the last metadata-unaware image (main 3c0a7a4).
// A rollback keeps the additive schema but these statements omit data_revisions.
const oldImageStateSQL = `
INSERT INTO user_data_state AS s
    (user_id, calc_version, has_data, data_from, data_to, last_operation_at, stale_after,
     full_months, recalculating, time_zone, calculated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (user_id) DO UPDATE
SET calc_version = EXCLUDED.calc_version,
    has_data = EXCLUDED.has_data,
    data_from = EXCLUDED.data_from,
    data_to = EXCLUDED.data_to,
    last_operation_at = EXCLUDED.last_operation_at,
    stale_after = EXCLUDED.stale_after,
    full_months = EXCLUDED.full_months,
    recalculating = EXCLUDED.recalculating,
    time_zone = EXCLUDED.time_zone,
    calculated_at = EXCLUDED.calculated_at,
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.user_id`

const oldImageHeadersSQL = `
INSERT INTO period_snapshot AS s
    (user_id, period_type, period_key, transfer_mode, calc_version, removed,
     range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at)
SELECT $1, p.period_type, p.period_key, p.transfer_mode, $2, p.removed,
       p.range_start, p.range_end, p.coverage, p.data_from, p.data_to, p.unread_lines_count, p.calculated_at
FROM unnest($3::smallint[], $4::text[], $5::smallint[], $6::boolean[],
            $7::timestamptz[], $8::timestamptz[], $9::smallint[], $10::timestamptz[], $11::timestamptz[],
            $12::integer[], $13::timestamptz[])
    AS p (period_type, period_key, transfer_mode, removed,
          range_start, range_end, coverage, data_from, data_to, unread_lines_count, calculated_at)
WHERE NOT p.removed OR NOT EXISTS (
    SELECT 1 FROM period_snapshot o
    WHERE o.user_id = $1 AND o.period_type = p.period_type AND o.period_key = p.period_key
      AND o.calc_version > $2)
ORDER BY p.period_type, p.period_key COLLATE "C", p.transfer_mode
ON CONFLICT (user_id, period_type, period_key, transfer_mode) DO UPDATE
SET calc_version = EXCLUDED.calc_version,
    removed = EXCLUDED.removed,
    range_start = EXCLUDED.range_start,
    range_end = EXCLUDED.range_end,
    coverage = EXCLUDED.coverage,
    data_from = EXCLUDED.data_from,
    data_to = EXCLUDED.data_to,
    unread_lines_count = EXCLUDED.unread_lines_count,
    calculated_at = EXCLUDED.calculated_at,
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.period_type, s.period_key, s.transfer_mode, s.removed`

func TestAPIRollbackWritesCannotCertifyOldSourceProvenance(t *testing.T) {
	rollbackProvenanceScenario(t)
}
func TestIntegrationRollbackWritesCannotCertifyOldSourceProvenance(t *testing.T) {
	rollbackProvenanceScenario(t)
}

func rollbackProvenanceScenario(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		state, header, empty, removal bool
	}{
		{"state_only", true, false, false, false},
		{"header_only", false, true, false, false},
		{"both_daily_same_source_vector", true, true, false, false},
		{"empty_list_no_data", true, false, true, false},
		{"removed_other_type_empty_no_data", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, c := startAPI(t)
			known := map[string]int64{"categories": 0, "uploads": 2, "time_zone": 0}
			key := "2026-09"
			if tc.empty {
				key = ""
			}
			initial := versionedRequest(alice, 1, key, known, known)
			initial.State.Recalculating = false
			initial.State.HasData = !tc.empty
			if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), initial); err != nil {
				t.Fatal(err)
			}
			check := func(want map[string]int64) {
				t.Helper()
				for _, selected := range []string{"2026-09", "2026-08"} {
					for _, mode := range []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT} {
						response, err := c.analytics.GetAnalytics(apitest.Caller(t.Context(), "assistant"), &pb.GetAnalyticsRequest{UserId: proto.String(alice), PeriodKey: proto.String(selected), TransferMode: mode})
						if err != nil || response.HasData == tc.empty {
							t.Fatal(response, err)
						}
						revisionsWant(t, response.DataRevisions, want)
					}
				}
				for _, period := range []common.PeriodType{common.PeriodType_PERIOD_TYPE_MONTH, common.PeriodType_PERIOD_TYPE_QUARTER} {
					list, err := c.analytics.ListPeriods(apitest.User(t.Context(), alice), &pb.ListPeriodsRequest{Period: period})
					if err != nil {
						t.Fatal(err)
					}
					revisionsWant(t, list.DataRevisions, want)
				}
			}
			check(known)
			// Both old-image statements commit in the same transaction when the
			// old request has both state and periods, just as the old handler did.
			if err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
				if tc.state {
					s := initial.State
					months := s.FullMonths
					if s.HasData {
						months++
					}
					var from, to, last, stale any
					if s.DataFrom != nil {
						from = s.DataFrom.AsTime()
					}
					if s.DataTo != nil {
						to = s.DataTo.AsTime()
					}
					if s.LastOperationAt != nil {
						last = s.LastOperationAt.AsTime()
					}
					if s.StaleAfter != nil {
						stale = s.StaleAfter.AsTime()
					}
					if _, err := env.App.DB.Writer(ctx).Exec(ctx, oldImageStateSQL, alice, int32(2), s.HasData, from, to, last, stale, months, false, s.TimeZone, s.CalculatedAt.AsTime()); err != nil {
						return err
					}
				}
				if tc.header {
					p := initial.Periods[0]
					// Same source counters, changed covered financial header, and
					// two modes: old SQL advances facts while retaining JSONB.
					_, err := env.App.DB.Writer(ctx).Exec(ctx, oldImageHeadersSQL, alice, int32(2), []int16{1, 1}, []string{key, key}, []int16{1, 2}, []bool{false, false}, []time.Time{p.Range.Start.AsTime(), p.Range.Start.AsTime()}, []time.Time{p.Range.End.AsTime(), p.Range.End.AsTime()}, []int16{int16(p.Bounds.Coverage), int16(p.Bounds.Coverage)}, []time.Time{p.Bounds.DataFrom.AsTime(), p.Bounds.DataFrom.AsTime()}, []time.Time{p.Bounds.DataTo.AsTime(), p.Bounds.DataTo.AsTime()}, []int32{1, 1}, []time.Time{p.CalculatedAt.AsTime(), p.CalculatedAt.AsTime()})
					return err
				}
				if tc.removal {
					// A newer tombstone of another type is absent from the selected
					// month inventory, but it still invalidates old global proof.
					_, err := env.App.DB.Writer(ctx).Exec(ctx, oldImageHeadersSQL, alice, int32(2), []int16{2, 2}, []string{"2026-Q3", "2026-Q3"}, []int16{1, 2}, []bool{true, true}, []*time.Time{nil, nil}, []*time.Time{nil, nil}, []*int16{nil, nil}, []*time.Time{nil, nil}, []*time.Time{nil, nil}, []*int32{nil, nil}, []*time.Time{nil, nil})
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Prove the fixture really kept the old stamp while changing the row.
			for _, storage := range []struct {
				enabled bool
				query   string
			}{
				{tc.state, `SELECT calc_version,(data_revisions->>'calc_version')::integer FROM user_data_state WHERE user_id=$1`},
				{tc.header, `SELECT calc_version,(data_revisions->>'calc_version')::integer FROM period_snapshot WHERE user_id=$1 LIMIT 1`},
			} {
				if !storage.enabled {
					continue
				}
				var row, stamp int32
				if err := env.App.DB.Primary().QueryRow(t.Context(), storage.query, alice).Scan(&row, &stamp); err != nil || row != 2 || stamp != 1 {
					t.Fatal("old image did not preserve stale metadata", row, stamp, err)
				}
			}
			check(map[string]int64{})
			// A new image can restore proof by writing a complete version. An
			// identical retry at that same version retains the existing contract.
			restored := versionedRequest(alice, 3, key, known, known)
			restored.State.HasData = !tc.empty
			restored.State.Recalculating = false
			for range 2 {
				if _, err := c.ingest.UpsertUserMetrics(producer(t.Context()), proto.Clone(restored).(*pb.UpsertUserMetricsRequest)); err != nil {
					t.Fatal(err)
				}
				check(known)
			}
		})
	}
}
