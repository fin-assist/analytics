package metrics_test

import (
	"context"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

// Frozen SQL of metadata-unaware main 3c0a7a4, executed through a real guarded
// repository transaction. These integration fixtures use no protobuf or RPC.
const preRevisionStateSQL = `
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

const preRevisionHeadersSQL = `
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

func TestIntegrationDomainRollbackWritesInvalidateStoredRevisionProof(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		state, header, empty, removal bool
	}{
		{"state_only", true, false, false, false},
		{"header_only", false, true, false, false},
		{"both_daily_same_vector", true, true, false, false},
		{"empty_no_data", true, false, true, false},
		{"removed_other_type", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t)
			user := userID(506)
			known := domain.Revisions{"categories": 0, "uploads": 2, "time_zone": 0}
			key := "2026-09"
			if tc.empty {
				key = ""
			}
			initial := revisionBatch(user, 1, key, known)
			initial.State.HasData = !tc.empty
			applyRevisionBatch(t, e, initial)
			checkRevisionRead(t, e, user, "2026-09", known, known, !tc.empty)
			if err := e.db.Run(t.Context(), func(ctx context.Context) error {
				alive, err := e.stones.Guard(ctx, user)
				if err != nil {
					return err
				}
				if !alive {
					t.Fatal("fixture user unexpectedly deleted")
				}
				if tc.state {
					s := initial.State
					months := s.FullMonths
					if s.HasData {
						months++
					}
					if _, err := e.db.Writer(ctx).Exec(ctx, preRevisionStateSQL, user, int32(2), s.HasData, s.DataFrom, s.DataTo, s.LastOperationAt, s.StaleAfter, months, false, s.TimeZone, s.CalculatedAt); err != nil {
						return err
					}
				}
				if tc.header {
					p := initial.Snapshots[0]
					if _, err := e.db.Writer(ctx).Exec(ctx, preRevisionHeadersSQL, user, int32(2), []int16{1, 1}, []string{key, key}, []int16{1, 2}, []bool{false, false}, []*time.Time{p.Range.Start, p.Range.Start}, []*time.Time{p.Range.End, p.Range.End}, []int16{int16(p.Coverage), int16(p.Coverage)}, []*time.Time{p.DataFrom, p.DataFrom}, []*time.Time{p.DataTo, p.DataTo}, []int32{2, 2}, []*time.Time{p.CalculatedAt, p.CalculatedAt}); err != nil {
						return err
					}
				}
				if tc.removal {
					if _, err := e.db.Writer(ctx).Exec(ctx, preRevisionHeadersSQL, user, int32(2), []int16{2, 2}, []string{"2026-Q3", "2026-Q3"}, []int16{1, 2}, []bool{true, true}, []*time.Time{nil, nil}, []*time.Time{nil, nil}, []*int16{nil, nil}, []*time.Time{nil, nil}, []*time.Time{nil, nil}, []*int32{nil, nil}, []*time.Time{nil, nil}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
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
				if err := e.db.Primary().QueryRow(t.Context(), storage.query, user).Scan(&row, &stamp); err != nil || row != 2 || stamp != 1 {
					t.Fatal("old SQL did not advance facts independently of stamp", row, stamp, err)
				}
			}
			var header domain.Revisions
			if !tc.header && !tc.empty {
				header = known
			}
			checkRevisionRead(t, e, user, "2026-09", nil, header, !tc.empty)
			checkRevisionRead(t, e, user, "2026-08", nil, nil, false)
			count := 1
			if tc.empty {
				count = 0
			}
			checkRevisionList(t, e, user, domain.PeriodMonth, count, nil)
			checkRevisionList(t, e, user, domain.PeriodQuarter, 0, nil)
			restored := revisionBatch(user, 3, key, known)
			restored.State.HasData = !tc.empty
			for range 2 {
				applyRevisionBatch(t, e, restored)
				checkRevisionRead(t, e, user, "2026-09", known, known, !tc.empty)
				checkRevisionRead(t, e, user, "2026-08", known, nil, false)
				checkRevisionList(t, e, user, domain.PeriodMonth, count, known)
				checkRevisionList(t, e, user, domain.PeriodQuarter, 0, known)
			}
		})
	}
}
