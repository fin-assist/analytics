package postgres

import (
	"context"
	"errors"

	"github.com/fin-assist/analytics/internal/domain"
)

// rollingPeriodsSQL reads one state/available-headers snapshot. ListPeriods has no
// transfer mode: return both headers in WITHOUT-first order for domain matching. The proof
// still checks every header/removal, including a partially delivered other mode.
const rollingPeriodsSQL = `
WITH proven_state AS MATERIALIZED (
    SELECT s.*, CASE WHEN NOT EXISTS (
        SELECT 1 FROM period_snapshot newer
        WHERE newer.user_id = s.user_id AND newer.calc_version > s.calc_version
    ) THEN s.data_revisions END AS proven_revisions
    FROM user_data_state s WHERE s.user_id = $1
)
SELECT s.time_zone,s.data_from,s.data_to,s.proven_revisions,s.calc_version,
       p.range_start,p.range_end,p.coverage,p.data_revisions,p.calc_version
FROM proven_state s
LEFT JOIN LATERAL (
    SELECT n.transfer_mode,n.range_start,n.range_end,n.coverage,n.data_revisions,n.calc_version
    FROM period_snapshot n
    WHERE n.user_id = s.user_id AND n.period_type = $2 AND n.period_key = $3 AND NOT n.removed
) p ON true
ORDER BY p.transfer_mode DESC`

// rollingPeriods never combines a new coverage anchor with an older singleton header.
func (r *Metrics) rollingPeriods(ctx context.Context, user string, typ domain.PeriodType) (domain.Periods, error) {
	data, err := readRollingPeriod(ctx, r.db.Reader(ctx), user, typ)
	if err != nil {
		return domain.Periods{}, err
	}
	periods, err := domain.RollingPeriods(typ, data.state, data.snapshots...)
	if err != nil {
		return domain.Periods{}, errors.New("analytics: invalid stored rolling boundary")
	}
	return periods, nil
}

type rollingPeriodRead struct {
	state     *domain.State
	snapshots []*domain.Snapshot
}

// Only list metadata is read; financial payloads remain specific to GetAnalytics' mode.
func readRollingPeriod(ctx context.Context, q querier, user string, typ domain.PeriodType) (rollingPeriodRead, error) {
	rows, err := q.Query(ctx, rollingPeriodsSQL, user, int16(typ), typ.String())
	if err != nil {
		return rollingPeriodRead{}, err
	}
	defer rows.Close()
	var out rollingPeriodRead
	for rows.Next() {
		state := &domain.State{}
		header := &domain.Snapshot{Range: &domain.TimeRange{}}
		var stateJSON, headerJSON []byte
		var stateVersion, headerVersion *int32
		var coverage *int16
		if err = rows.Scan(&state.TimeZone, &state.DataFrom, &state.DataTo, &stateJSON, &stateVersion,
			&header.Range.Start, &header.Range.End, &coverage, &headerJSON, &headerVersion); err != nil {
			return rollingPeriodRead{}, err
		}
		state.Revisions, err = revisionsFromJSON(stateJSON, stateVersion)
		if err != nil {
			return rollingPeriodRead{}, err
		}
		out.state = state
		if headerVersion == nil {
			continue
		}
		if header.Range.Start == nil || header.Range.End == nil || coverage == nil {
			return rollingPeriodRead{}, errBrokenHeader
		}
		header.Coverage = domain.Coverage(*coverage)
		header.Revisions, err = revisionsFromJSON(headerJSON, headerVersion)
		if err != nil {
			return rollingPeriodRead{}, err
		}
		out.snapshots = append(out.snapshots, header)
	}
	return out, rows.Err()
}
