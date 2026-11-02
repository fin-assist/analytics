package metrics

import (
	"context"
	"errors"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

// ChangesUsecase coordinates atomic marker delivery and local composition.
type ChangesUsecase interface {
	Mark(context.Context, string, int32) error
	Apply(context.Context, string, int32, func(context.Context) error) error
}

func (h *ingest) MarkRecalculating(ctx context.Context, req *analyticsv1.MarkRecalculatingRequest) (*analyticsv1.MarkRecalculatingResponse, error) {
	if h.changes == nil {
		return nil, errors.New("changes not wired")
	}
	err := h.changes.Mark(ctx, canonicalUserID(req.GetUserId()), req.GetCalcVersion())
	if errors.Is(err, domain.ErrInvalid) {
		return nil, invalidArgument()
	}
	if err != nil {
		return nil, err
	}
	return &analyticsv1.MarkRecalculatingResponse{}, nil
}

func (h *analytics) ApplyRegularPaymentDismissal(ctx context.Context, req *analyticsv1.ApplyRegularPaymentDismissalRequest) (*analyticsv1.ApplyRegularPaymentDismissalResponse, error) {
	user, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, invalidArgument()
	}
	p, err := regularProjectionOf(req, canonicalUserID(user))
	if err != nil {
		return nil, invalidArgument()
	}
	if h.changes == nil {
		return nil, errors.New("changes not wired")
	}
	var blocks []*commonv1.Block
	err = h.changes.Apply(ctx, canonicalUserID(user), req.GetCalcVersion(), func(txctx context.Context) error {
		var e error
		blocks, e = guardBlock(txctx, func(ctx context.Context) ([]*commonv1.Block, error) { return p.insight(ctx, "regular_payments") })
		return e
	})
	if errors.Is(err, domain.ErrInvalid) {
		return nil, invalidArgument()
	}
	if errors.Is(err, usecase.ErrDeleted) {
		return nil, finerr.TokenRevoked()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, finerr.WithRequestRetryInfo(finerr.InternalError(), true)
	}
	return &analyticsv1.ApplyRegularPaymentDismissalResponse{Params: req.Params, Currency: req.Projection.Currency, TimeZone: req.Projection.TimeZone, Blocks: blocks, RequiredCalcVersion: req.CalcVersion}, nil
}
