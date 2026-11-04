package metrics

import (
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
)

func TestUnitDailyMethodologyPreservesReadyAndTentativeValuesAndActions(t *testing.T) {
	const tileID = "daily_expense"
	const tileTitle = "Средние расходы в день"
	const captionKey = "caption"
	const valueKey = "value"
	const statusKey = "status"
	const actionKey = "action"
	const comparison = "в августе — 2 890 ₽"
	p := composition{zone: time.UTC}
	for _, status := range []analyticsv1.MetricStatus{analyticsv1.MetricStatus_METRIC_STATUS_READY, analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE} {
		for _, value := range []int64{0, -101, 303} {
			m := &analyticsv1.Metric{Status: status, ValueMinor: &value, Filters: &commonv1.OperationsFilter{
				Kind: commonv1.KindFilter_KIND_FILTER_EXPENSE, TransferMode: commonv1.TransferMode_TRANSFER_MODE_WITH,
			}}
			tile := p.tile(tileID, tileTitle, m, comparison)
			wantStatus := "ready"
			if status == analyticsv1.MetricStatus_METRIC_STATUS_TENTATIVE {
				wantStatus = "tentative"
			}
			if tile["help_text"] != expectedDailyHelpText || tile[captionKey] != comparison || tile[valueKey] != value || tile[statusKey] != wantStatus {
				t.Fatalf("text relocation changed daily facts: %v", tile)
			}
			action, ok := tile[actionKey].(map[string]any)
			if !ok || action["type"] != "open_operations" {
				t.Fatalf("daily action lost: %v", tile)
			}
			filter := action["filter"].(map[string]any)
			if filter["kind"] != "expense" || filter["transfer_mode"] != "with" {
				t.Fatalf("daily action changed: %v", filter)
			}
		}
	}
	// Unavailable data remains unavailable and never gains a made-up value or action.
	tile := p.tile(tileID, tileTitle, &analyticsv1.Metric{Status: analyticsv1.MetricStatus_METRIC_STATUS_UNAVAILABLE}, comparison)
	if tile[statusKey] != "unavailable" || tile[valueKey] != nil || tile[actionKey] != nil || tile[captionKey] != nil {
		t.Fatalf("unavailable daily value invented: %v", tile)
	}
}
