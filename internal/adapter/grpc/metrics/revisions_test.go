package metrics

import (
	"errors"
	"math"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitRevisionConversionPreservesOpenCodesWithoutLoss(t *testing.T) {
	in := []*common.DataRevision{{Code: "uploads", Value: 0}, {Code: "future_source", Value: math.MaxInt64}}
	values, err := revisionsOf(in)
	if err != nil || values["future_source"] != math.MaxInt64 {
		t.Fatal(values, err)
	}
	out := revisionMessages(values)
	if len(out) != 2 || out[0].Code != "future_source" || out[0].Value != math.MaxInt64 || out[1].Code != "uploads" || out[1].Value != 0 {
		t.Fatal(out)
	}
	for _, bad := range [][]*common.DataRevision{{nil}, {{Code: "uploads", Value: 1}, {Code: "uploads", Value: 1}}, {{Code: "uploads", Value: -1}}} {
		if _, err := revisionsOf(bad); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal(bad, err)
		}
	}
	many := make([]*common.DataRevision, 33)
	if _, err := revisionsOf(many); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if values, err := revisionsOf(nil); err != nil || len(values) != 0 {
		t.Fatal(values, err)
	}
}

func TestUnitResponseProvenanceIntersectsEveryUsedSource(t *testing.T) {
	screen := screenFixture(t)
	screen.State.Revisions = domain.Revisions{"uploads": 4, "categories": 1, "future_source": math.MaxInt64}
	screen.Snapshot.Revisions = domain.Revisions{"uploads": 5, "categories": 1, "future_source": math.MaxInt64}
	f := &fakeScreen{result: screen}
	h := &analytics{screen: f, composer: composerForTest(t)}
	ctx := reqctx.WithUserID(t.Context(), user)
	got, err := h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if err != nil || len(got.DataRevisions) != 2 || got.DataRevisions[0].Code != "categories" || got.DataRevisions[1].Value != math.MaxInt64 {
		t.Fatal(got, err)
	}
	f.result.State.Revisions = nil
	got, err = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if err != nil || len(got.DataRevisions) != 0 {
		t.Fatal("legacy state certified snapshot metadata", got, err)
	}
	f.result.State.Revisions = screen.Snapshot.Revisions
	f.result.Snapshot = nil
	got, err = h.GetAnalytics(ctx, &analyticsv1.GetAnalyticsRequest{})
	if err != nil || len(got.DataRevisions) != 3 {
		t.Fatal("missing snapshot lost state evidence", got, err)
	}
}
