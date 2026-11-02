package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/analytics/internal/domain"
)

const chartSize = 60

// The JSON array stays in PostgreSQL; only the requested sixty points cross the DB boundary.
const chartHeadSQL = `SELECT p.calc_version,coalesce(p.time_zone,''),
 CASE WHEN jsonb_typeof(m.details->'monthly_chart'->'points')='array' THEN jsonb_array_length(m.details->'monthly_chart'->'points') ELSE 0 END,
 coalesce(m.details->'monthly_chart'->'points'->0->>'month',''),p.data_revisions,p.range_start,p.range_end,
 s.calc_version,s.time_zone,s.has_data
 FROM period_snapshot p JOIN user_data_state s ON s.user_id=p.user_id
 JOIN period_metric m ON m.user_id=p.user_id AND m.period_type=p.period_type
 AND m.period_key=p.period_key AND m.transfer_mode=p.transfer_mode
 WHERE p.user_id=$1 AND p.period_type=4 AND p.period_key='all_time' AND p.transfer_mode=$2
 AND NOT p.removed AND m.metric_id='monthly_chart' AND m.status=1`

func (r *Metrics) chartPage(ctx context.Context, user string, mode domain.TransferMode, selected *string, wantRevision string, page int) (*domain.ChartPage, error) {
	q := r.db.Writer(ctx)
	var version, stateVersion, total int
	var zone, stateZone, first string
	var revisions []byte
	var start, end time.Time
	var hasData bool
	err := q.QueryRow(ctx, chartHeadSQL, user, int16(mode)).Scan(&version, &zone, &total, &first, &revisions, &start, &end, &stateVersion, &stateZone, &hasData)
	if errors.Is(err, pgx.ErrNoRows) {
		if wantRevision != "" {
			return nil, domain.ErrCursorRange
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hasData {
		if wantRevision != "" {
			return nil, domain.ErrCursorRange
		}
		return nil, nil
	}
	if total < 1 {
		if wantRevision != "" {
			return nil, domain.ErrCursorRange
		}
		return &domain.ChartPage{TimeZone: zone}, nil // Malformed delivered details degrade the graph, not healthy facts.
	}
	if zone == "" || zone != stateZone {
		if wantRevision != "" {
			return nil, domain.ErrCursorRange
		}
		return nil, nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	firstMonth, valid := monthIndex(first)
	if !valid {
		if wantRevision != "" {
			return nil, domain.ErrCursorRange
		}
		return &domain.ChartPage{TimeZone: zone}, nil
	}
	// The snapshot version includes all metric inputs. The civil day also changes point completeness.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s|%s|%s|%s|%s|%d", user, mode, version, zone, revisions, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), r.now().In(loc).Format(time.DateOnly), total)))
	rev := hex.EncodeToString(digest[:])
	if wantRevision != "" && wantRevision != rev {
		return nil, domain.ErrCursorRange
	}
	if wantRevision == "" {
		page = 0
		if selected != nil {
			selectedMonth, valid := monthIndex(*selected)
			if !valid {
				return nil, errors.New("invalid selected chart month")
			}
			index := max(0, min(total-1, selectedMonth-firstMonth))
			page = (total - 1 - index) / chartSize
		}
	}
	if page < 0 || page > (total-1)/chartSize {
		return nil, domain.ErrCursorRange
	}
	hi := total - page*chartSize
	lo := max(0, hi-chartSize)
	var details []byte
	err = q.QueryRow(ctx, `SELECT jsonb_build_object('monthly_chart',jsonb_build_object('points',
 (SELECT jsonb_agg(m.details->'monthly_chart'->'points'->i ORDER BY i) FROM generate_series($3::integer,$4::integer-1) i)))
 FROM period_metric m WHERE user_id=$1 AND period_type=4 AND period_key='all_time' AND transfer_mode=$2 AND metric_id='monthly_chart'`, user, int16(mode), lo, hi).Scan(&details)
	if err != nil {
		return nil, err
	}
	out := &domain.ChartPage{Details: details, TimeZone: zone, Revision: rev}
	if lo > 0 {
		out.Previous, err = r.chartCursor(ctx, user, mode, rev, page+1)
		if err != nil {
			return nil, err
		}
	}
	if page > 0 {
		out.Next, err = r.chartCursor(ctx, user, mode, rev, page-1)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func monthIndex(key string) (int, bool) {
	if (domain.PeriodKey{Type: domain.PeriodMonth, Key: key}).Validate() != nil {
		return 0, false
	}
	y, _ := strconv.Atoi(key[:4])
	m, _ := strconv.Atoi(key[5:])
	return y*12 + m - 1, true
}

func (r *Metrics) chartCursor(ctx context.Context, user string, mode domain.TransferMode, revision string, page int) (string, error) {
	q := r.db.Writer(ctx)
	// Existing pointers are immutable: a normal read must not create another row version.
	var result string
	err := q.QueryRow(ctx, `SELECT token FROM chart_cursor WHERE user_id=$1 AND transfer_mode=$2 AND revision=$3 AND page=$4 AND created_at>=now()-interval '1 day'`, user, int16(mode), revision, page).Scan(&result)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	// Initial insertion and expiry cleanup can race; the owner retries the entire RR snapshot.
	if _, err := q.Exec(ctx, `DELETE FROM chart_cursor WHERE user_id=$1 AND created_at<now()-interval '1 day'`, user); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	err = q.QueryRow(ctx, `INSERT INTO chart_cursor(user_id,transfer_mode,revision,page,token) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(user_id,transfer_mode,revision,page) DO NOTHING RETURNING token`, user, int16(mode), revision, page, token).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT token FROM chart_cursor WHERE user_id=$1 AND transfer_mode=$2 AND revision=$3 AND page=$4`, user, int16(mode), revision, page).Scan(&result)
	}
	return result, err
}

// chartSnapshot retries only serialization conflicts of the whole read + cursor transaction.
// It never repeats an external effect or joins/retries an aborted outer transaction.
func (r *Metrics) chartSnapshot(ctx context.Context, read func(context.Context) error) error {
	attempts := 3
	if _, nested := Tx(ctx); nested {
		attempts = 1
	}
	var err error
	for range attempts {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = r.db.RunWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, read)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
			return err
		}
	}
	return err
}

// ReadChartPage validates ownership against the verified caller, and reads the current primary snapshot.
func (r *Metrics) ReadChartPage(ctx context.Context, user, cursor string) (*domain.ChartPage, error) {
	if !domain.ValidUserID(user) || len(cursor) != 64 {
		return nil, domain.ErrCursorFormat
	}
	if _, err := hex.DecodeString(cursor); err != nil {
		return nil, domain.ErrCursorFormat
	}
	var out *domain.ChartPage
	err := r.chartSnapshot(ctx, func(ctx context.Context) error {
		out = nil
		var mode int16
		var revision string
		var page int
		err := r.db.Writer(ctx).QueryRow(ctx, `SELECT transfer_mode,revision,page FROM chart_cursor WHERE user_id=$1 AND token=$2 AND created_at>=now()-interval '1 day'`, user, cursor).Scan(&mode, &revision, &page)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCursorFormat
		}
		if err != nil {
			return err
		}
		out, err = r.chartPage(ctx, user, domain.TransferMode(mode), nil, revision, page)
		return err
	})
	return out, err
}
