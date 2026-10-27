package metrics

import (
	"errors"
	"math"
	"testing"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

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
