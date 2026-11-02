package postgres

import (
	"os"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
)

func TestIntegrationRollingStateKeepsItsOwnZoneBeforePeriodDelivery(t *testing.T) {
	pool := openDB(t)
	user := newUser(420)
	raw, err := os.ReadFile("../../../testdata/rolling.json")
	if err != nil {
		t.Fatal(err)
	}
	req := &analyticsv1.UpsertUserMetricsRequest{}
	if err = protojson.Unmarshal(raw, req); err != nil {
		t.Fatal(err)
	}
	state := validState()
	state.dataFrom, state.dataTo = ptr(req.State.DataFrom.AsTime()), ptr(req.State.DataTo.AsTime())
	state.lastOperation, state.calculatedAt = ptr(req.State.LastOperationAt.AsTime()), ptr(req.State.CalculatedAt.AsTime())
	state.stale = ptr(req.State.StaleAfter.AsTime())
	if _, err = upsertState(t.Context(), pool, user, 4, state); err != nil {
		t.Fatal(err)
	}
	if _, err = upsertProto(t.Context(), pool, user, 4, req.Periods, nil); err != nil {
		t.Fatal(err)
	}
	state.timeZone, state.recalculating = "UTC", true
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	state.dataTo = &end
	if _, err = upsertState(t.Context(), pool, user, 5, state); err != nil {
		t.Fatal(err)
	}
	repo := NewMetrics(testMetricsDB(pool), time.Now)
	screen, err := metrics.NewScreen(repo, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []domain.PeriodType{domain.PeriodRollingQuarter, domain.PeriodRollingYear} {
		key := typ.String()
		for _, mode := range []domain.TransferMode{domain.TransferWith, domain.TransferWithout} {
			stored, err := repo.ReadAnalytics(t.Context(), user, typ, &key, mode)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State.TimeZone != "UTC" || stored.Snapshot.TimeZone != "Europe/Moscow" || !stored.State.DataTo.Equal(end) {
				t.Fatalf("state/snapshot zones = %s/%s, anchor = %v", stored.State.TimeZone, stored.Snapshot.TimeZone, stored.State.DataTo)
			}
			got, err := screen.Get(t.Context(), user, typ, &key, mode)
			if err != nil || got.Snapshot != nil || !got.Range.End.Equal(end) || !got.Recalculating {
				t.Fatalf("pending rolling delivery: %+v %v", got, err)
			}
		}
	}
}
