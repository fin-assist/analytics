package metrics

import (
	"context"
	"errors"

	"github.com/fin-assist/analytics/internal/domain"
)

type ChartStore interface {
	ReadChartPage(context.Context, string, string) (*domain.ChartPage, error)
}

// GetChartPage keeps identity outside the cursor and uses the same primary repository as Get.
func (s *Screen) GetChartPage(ctx context.Context, user, cursor string) (*domain.ChartPage, error) {
	if !domain.ValidUserID(user) {
		return nil, domain.ErrInvalid
	}
	store, ok := s.store.(ChartStore)
	if !ok {
		return nil, errors.New("chart store not wired")
	}
	return store.ReadChartPage(ctx, user, cursor)
}
