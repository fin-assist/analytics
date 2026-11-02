package postgres

import "context"

const purgeMarkerSQL = `DELETE FROM recalculation_marker WHERE user_id=$1`

// MarkRecalculating advances only the marker under the caller's tombstone guard.
func (r *Metrics) MarkRecalculating(ctx context.Context, user string, version int32) error {
	tx, err := r.transaction(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO recalculation_marker(user_id,requested_version) VALUES($1,$2)
 ON CONFLICT(user_id) DO UPDATE SET requested_version=greatest(recalculation_marker.requested_version,excluded.requested_version)`, user, version)
	return err
}
