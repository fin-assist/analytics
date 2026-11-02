package metrics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/domain"
)

type chartStoreFake struct {
	screenStore
	page         *domain.ChartPage
	chartErr     error
	ctx          context.Context
	user, cursor string
	chartCalls   int
}

func (f *chartStoreFake) ReadChartPage(ctx context.Context, user, cursor string) (*domain.ChartPage, error) {
	f.ctx, f.user, f.cursor = ctx, user, cursor
	f.chartCalls++
	return f.page, f.chartErr
}

func TestUnitScreenGetChartPageDelegatesWithoutChangingPageOrErrors(t *testing.T) {
	page := &domain.ChartPage{TimeZone: "Europe/Moscow", Revision: "r1", Previous: "older", Details: []byte(`{"monthly_chart":{"points":[]}}`)}
	for _, tc := range []struct {
		name string
		page *domain.ChartPage
		err  error
	}{
		{"success", page, nil},
		{"format", nil, fmt.Errorf("cursor: %w", domain.ErrCursorFormat)},
		{"range", nil, fmt.Errorf("cursor: %w", domain.ErrCursorRange)},
		{"storage", nil, errors.New("storage unavailable")},
		{"cancelled", nil, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &chartStoreFake{page: tc.page, chartErr: tc.err}
			screen, err := NewScreen(store, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			got, err := screen.GetChartPage(ctx, user, "opaque-cursor")
			if got != tc.page || !errors.Is(err, tc.err) {
				t.Fatalf("page=%+v err=%v; want the supplied page and error", got, err)
			}
			if store.chartCalls != 1 || store.ctx != ctx || store.user != user || store.cursor != "opaque-cursor" || store.calls != 0 {
				t.Fatalf("chart call=%+v; no ordinary screen read is expected", store)
			}
		})
	}
}

func TestUnitScreenGetChartPageRejectsInvalidIdentityBeforeStorage(t *testing.T) {
	store := &chartStoreFake{}
	screen, err := NewScreen(store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "not-a-user"} {
		got, err := screen.GetChartPage(t.Context(), invalid, "opaque-cursor")
		if got != nil || !errors.Is(err, domain.ErrInvalid) || store.chartCalls != 0 {
			t.Fatalf("invalid identity: page=%+v err=%v calls=%d", got, err, store.chartCalls)
		}
	}
	withoutChart, err := NewScreen(&screenStore{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := withoutChart.GetChartPage(t.Context(), user, "opaque-cursor"); got != nil || err == nil {
		t.Fatalf("missing chart store: page=%+v err=%v", got, err)
	}
}
