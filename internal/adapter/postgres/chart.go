package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

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
		return nil, domain.ErrCursorFormat
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
		out.Previous = encodeChartCursor(user, mode, rev, page+1)
	}
	if page > 0 {
		out.Next = encodeChartCursor(user, mode, rev, page-1)
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

// chartCursor is a private position descriptor, never an authorization credential.
// Identity always comes from the verified caller; owner only distinguishes foreign from stale cursors.
type chartCursor struct {
	Version  int                 `json:"v"`
	Owner    string              `json:"owner"`
	Mode     domain.TransferMode `json:"mode"`
	Revision string              `json:"revision"`
	Page     int                 `json:"page"`
}

func chartOwner(user string) string {
	digest := sha256.Sum256([]byte(user))
	return hex.EncodeToString(digest[:])
}

func encodeChartCursor(user string, mode domain.TransferMode, revision string, page int) string {
	value := chartCursor{Version: 1, Owner: chartOwner(user), Mode: mode, Revision: revision, Page: page}
	// A struct of scalar integers/strings cannot fail JSON encoding.
	raw, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeChartCursor(user, encoded string) (chartCursor, error) {
	var value chartCursor
	if !domain.ValidUserID(user) || len(encoded) == 0 || len(encoded) > 2048 {
		return value, domain.ErrCursorFormat
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &value) != nil {
		return value, domain.ErrCursorFormat
	}
	// The server issues one canonical private format: reject missing/duplicate fields and trailing input.
	canonical, _ := json.Marshal(value)
	if !bytes.Equal(raw, canonical) || value.Version != 1 || value.Owner != chartOwner(user) ||
		(value.Mode != domain.TransferWith && value.Mode != domain.TransferWithout) || value.Page < 0 || len(value.Revision) != 64 {
		return value, domain.ErrCursorFormat
	}
	if _, err = hex.DecodeString(value.Revision); err != nil {
		return value, domain.ErrCursorFormat
	}
	return value, nil
}

// ReadChartPage validates ownership before revision and reads the current primary snapshot without writes.
func (r *Metrics) ReadChartPage(ctx context.Context, user, cursor string) (*domain.ChartPage, error) {
	value, err := decodeChartCursor(user, cursor)
	if err != nil {
		return nil, err
	}
	var out *domain.ChartPage
	err = r.db.RunWith(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context) error {
		var err error
		out, err = r.chartPage(ctx, user, value.Mode, nil, value.Revision, value.Page)
		return err
	})
	return out, err
}
