package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-grpc/server"

	"github.com/fin-assist/analytics/internal/domain"
)

// The access table is grpc-api.md 3.3 for the three services analytics serves.
func TestUnitAccessPolicyMatchesContract(t *testing.T) {
	want := map[string]server.Rule{
		"/finassist.analytics.v1.AnalyticsService/*":       {Callers: []string{"bank-bff", "assistant"}, Model: server.ModelMixed},
		"/finassist.analytics.v1.MetricsIngestService/*":   {Callers: []string{"bank-statement-data"}, Model: server.ModelSystem},
		"/finassist.userdeletion.v1.UserDeletionService/*": {Callers: []string{"user"}, Model: server.ModelSystem},
	}
	got := AccessPolicy()
	if len(got) != len(want) {
		t.Fatalf("policy %v", got)
	}
	for method, rule := range want {
		r, ok := got[method]
		if !ok || r.Model != rule.Model || strings.Join(r.Callers, ",") != strings.Join(rule.Callers, ",") || r.AllowDeleted {
			t.Fatalf("%s: %+v, want %+v", method, r, rule)
		}
	}
	sources := MigrationSources()
	if len(sources) != 2 || sources[0].Name != "tombstone" || sources[0].Table != "tombstone_schema_migrations" || sources[1].Table != "schema_migrations" {
		t.Fatalf("migration sources %+v: the library goes first, with its own table", sources)
	}
}

func TestUnitIncompletePeriodsCountsAndWarnsWithoutUserData(t *testing.T) {
	if _, err := newIncompletePeriods(nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("nil registry accepted")
	}
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	o, err := newIncompletePeriods(reg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.IncompletePeriod(t.Context(), domain.PeriodKey{Type: domain.PeriodQuarter, Key: "2026-Q3"})
	o.IncompletePeriod(t.Context(), domain.PeriodKey{Type: domain.PeriodQuarter, Key: "2026-Q2"})
	o.IncompletePeriod(t.Context(), domain.PeriodKey{Type: domain.PeriodMonth, Key: "2026-09"})
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "finassist_incomplete_period_snapshots_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			counts[m.GetLabel()[0].GetName()+"="+m.GetLabel()[0].GetValue()] = m.GetCounter().GetValue()
		}
	}
	if counts["period_type=quarter"] != 2 || counts["period_type=month"] != 1 || len(counts) != 2 {
		t.Fatalf("counts %v, want quarter 2 and month 1", counts)
	}
	if !strings.Contains(logs.String(), `"period_key":"2026-Q3"`) || strings.Contains(logs.String(), "user") {
		t.Fatalf("log %s", logs.String())
	}
	if _, err := newIncompletePeriods(reg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}
