package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitChartRejectsMalformedIdentityAndCursorBeforeDatabase(t *testing.T) {
	repo := &Metrics{}
	for _, tc := range []struct{ user, cursor string }{{"bad", strings.Repeat("a", 64)}, {newUser(416), ""}, {newUser(416), "short"}, {newUser(416), strings.Repeat("z", 64)}} {
		if _, err := repo.ReadChartPage(t.Context(), tc.user, tc.cursor); !errors.Is(err, domain.ErrCursorFormat) {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", "all_time", "2026-00", "2026-13"} {
		if _, err := monthIndex(key); err == nil {
			t.Fatal("invalid month accepted", key)
		}
	}
	dec, err := monthIndex("2025-12")
	if err != nil {
		t.Fatal(err)
	}
	jan, err := monthIndex("2026-01")
	if err != nil || jan-dec != 1 {
		t.Fatal("calendar adjacency changed", jan, dec, err)
	}
}

func TestUnitMarkerRequiresExistingLocalTransaction(t *testing.T) {
	repo := NewMetrics(&DB{}, nil)
	if err := repo.MarkRecalculating(t.Context(), newUser(416), 1); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
}
