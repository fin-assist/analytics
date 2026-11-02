package app_test

import (
	"os"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/analytics/internal/app/apitest"
)

// The wire fixture was exported from a real BSD Dismiss API/PG18 run using synthetic OFX.
// Independent expectations and provenance are in bsd_dismissal_projection_facts.json.
func TestAPIAcceptsActualBSDV11Projection(t *testing.T) {
	env, c := startAPI(t)
	raw, err := os.ReadFile("../../testdata/bsd_dismissal_projection.pb")
	if err != nil {
		t.Fatal(err)
	}
	p := &analyticsv1.MetricProjection{}
	if err = proto.Unmarshal(raw, p); err != nil {
		t.Fatal(err)
	}
	user := "73ef1f95-14d5-453c-80ec-d0e2ac8c854d"
	ctx := apitest.User(t.Context(), user)
	req := &analyticsv1.ApplyRegularPaymentDismissalRequest{RegularPaymentId: "01a19bb3-819e-7309-a19b-d3122d59c156", CalcVersion: 1, Params: p.Params, Projection: p}
	got, err := c.analytics.ApplyRegularPaymentDismissal(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Blocks) != 1 || got.RequiredCalcVersion != 1 || got.TimeZone != "Europe/Moscow" || got.Currency != "RUB" || got.Blocks[0].Status != "ready" {
		t.Fatal(got)
	}
	d := got.Blocks[0].Data.AsMap()
	items := d["items"].([]any)
	if d["value"] != float64(100000) || !strings.Contains(d["description"].(string), "1 списание") || len(items) != 1 || items[0].(map[string]any)["title"] != "Taxi" || items[0].(map[string]any)["value"] != float64(100000) || d["items_more"] != nil {
		t.Fatal(d)
	}
	screen, err := c.analytics.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{Period: 1, PeriodKey: proto.String("2026-09"), TransferMode: 1})
	if err != nil || screen.HasData || screen.Calculation.GetCalcVersion() != 0 || !screen.Calculation.GetRecalculating() {
		t.Fatal(screen, err)
	}
	var n int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM period_snapshot)+(SELECT count(*) FROM user_data_state)`).Scan(&n); err != nil || n != 0 {
		t.Fatal("supplied projection wrote global state", n, err)
	}
}
