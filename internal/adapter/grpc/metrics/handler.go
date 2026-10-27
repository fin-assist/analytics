// Package metrics is the gRPC delivery of the stored metrics (FIN-283):
// MetricsIngestService.UpsertUserMetrics from bank-statement-data and
// AnalyticsService.ListPeriods from bank-bff and assistant. Handlers convert messages and
// call the usecase; identity and the access table are the server's interceptors
// (grpc-api.md 3.3). GetAnalytics resolves and composes stored metrics (FIN-5).
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/analytics/internal/domain"
	usecase "github.com/fin-assist/analytics/internal/metrics"
)

// Usecase is what the handlers call (internal/metrics.Metrics).
type Usecase interface {
	Upsert(ctx context.Context, u domain.Upsert) (usecase.Result, error)
	Periods(ctx context.Context, userID string, typ domain.PeriodType) (domain.Periods, error)
}

// ScreenUsecase resolves the persisted screen without calculating financial metrics.
type ScreenUsecase interface {
	Get(context.Context, string, domain.PeriodType, *string, domain.TransferMode) (domain.Analytics, error)
}

// Register registers MetricsIngestService and AnalyticsService on s. log records the
// refused batches of the producer without their content.
func Register(s *grpc.Server, uc Usecase, screen ScreenUsecase, composer *Composer, log *slog.Logger) {
	analyticsv1.RegisterMetricsIngestServiceServer(s, &ingest{uc: uc, log: log})
	analyticsv1.RegisterAnalyticsServiceServer(s, &analytics{uc: uc, screen: screen, composer: composer})
}

// invalidArgument is the bare refusal of a request that breaks the rules of the contract
// (grpc-api.md 5.1, rule 3): no ErrorInfo, no text of the cause.
func invalidArgument() error {
	return status.Error(codes.InvalidArgument, codes.InvalidArgument.String()) //nolint:forbidigo // Bare internal refusal, grpc-api.md 5.1 rule 3 and 7.3: no client reaction exists for a producer or caller error.
}

type ingest struct {
	analyticsv1.UnimplementedMetricsIngestServiceServer
	uc  Usecase
	log *slog.Logger
}

// UpsertUserMetrics applies a batch of bank-statement-data. A batch that breaks a rule is
// refused whole with a bare INVALID_ARGUMENT: the outbox row stays and is seen in the alert.
// A deleted user's batch is acknowledged without effects.
func (h *ingest) UpsertUserMetrics(ctx context.Context, req *analyticsv1.UpsertUserMetricsRequest) (*analyticsv1.UpsertUserMetricsResponse, error) {
	u, err := upsertOf(req)
	if err == nil {
		var result usecase.Result
		if result, err = h.uc.Upsert(ctx, u); err == nil {
			return &analyticsv1.UpsertUserMetricsResponse{
				AppliedPeriods: int32(result.Applied), SkippedStalePeriods: int32(result.Skipped), //nolint:gosec // At most the snapshots of one 4 MiB call.
			}, nil
		}
	}
	if errors.Is(err, domain.ErrInvalid) {
		// The rule, not the data: the message names keys and fields of the batch only.
		h.log.ErrorContext(ctx, "metrics batch refused", "calc_version", req.GetCalcVersion(), "rule", bounded(err.Error()))
		return nil, invalidArgument()
	}
	return nil, err
}

// maxRuleLog bounds the refused rule in the log: it quotes keys and metric IDs of the producer.
const maxRuleLog = 256

func bounded(rule string) string {
	if len(rule) <= maxRuleLog {
		return rule
	}
	return strings.ToValidUTF8(rule[:maxRuleLog], "") + "…"
}

type analytics struct {
	analyticsv1.UnimplementedAnalyticsServiceServer
	uc       Usecase
	screen   ScreenUsecase
	composer *Composer
}

// ListPeriods answers both models of the method (grpc-api.md 3.2): with a user token the
// user is the verified sub and the request user_id is empty or equal to it; a system call
// of assistant names the user in user_id.
func (h *analytics) ListPeriods(ctx context.Context, req *analyticsv1.ListPeriodsRequest) (*analyticsv1.ListPeriodsResponse, error) {
	userID, ok := subject(ctx, req.UserId)
	if !ok {
		return nil, invalidArgument()
	}
	typ := domain.PeriodType(req.GetPeriod())
	if req.GetPeriod() == commonv1.PeriodType_PERIOD_TYPE_UNSPECIFIED {
		typ = domain.PeriodMonth
	}
	if !typ.Known() {
		return nil, finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "period", Description: "unknown period type"})
	}
	periods, err := h.uc.Periods(ctx, userID, typ)
	if errors.Is(err, domain.ErrInvalid) {
		return nil, invalidArgument()
	}
	if err != nil {
		return nil, err
	}
	resp := &analyticsv1.ListPeriodsResponse{TimeZone: periods.TimeZone, Periods: make([]*analyticsv1.PeriodItem, 0, len(periods.Items))}
	for _, p := range periods.Items {
		resp.Periods = append(resp.Periods, &analyticsv1.PeriodItem{
			Key:      p.Key,
			Range:    &commonv1.TimeRange{Start: timestamppb.New(p.Start), End: timestamppb.New(p.End)},
			Coverage: commonv1.Coverage(p.Coverage),
		})
	}
	return resp, nil
}

// subject is the user of a mixed-model call. The verified sub wins and a different user_id
// is refused, never followed; without a sub (a system call admitted by the access table)
// user_id is required.
// Both sides are canonicalized first, so the comparison ignores the case of the UUID.
func subject(ctx context.Context, requested *string) (string, bool) {
	want := ""
	if requested != nil {
		want = canonicalUserID(*requested)
	}
	if sub, ok := reqctx.UserID(ctx); ok {
		sub = canonicalUserID(sub)
		if want != "" && want != sub {
			return "", false
		}
		return sub, true
	}
	if want == "" {
		return "", false
	}
	return want, true
}
