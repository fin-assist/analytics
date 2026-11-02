package metrics

import (
	"context"
	"errors"
	"strings"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/fin-assist/analytics/internal/domain"
)

const chartHelp = "Столбики показывают полные календарные месяцы всей загруженной истории. Прокрутите к краю графика, чтобы загрузить соседние месяцы. Нажмите на месяц с данными, чтобы открыть его аналитику. Итоги и выводы экрана относятся к выбранному периоду."

type chartReader interface {
	GetChartPage(context.Context, string, string) (*domain.ChartPage, error)
}

func (h *analytics) GetChartPage(ctx context.Context, req *analyticsv1.GetChartPageRequest) (*analyticsv1.GetChartPageResponse, error) {
	user, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, invalidArgument()
	}
	invalid := func(reason string) error {
		return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "cursor", Reason: reason, Description: "invalid chart cursor"})
	}
	if req.GetCursor() == "" {
		return nil, invalid("REQUIRED")
	}
	reader, ok := h.screen.(chartReader)
	if !ok {
		return nil, errors.New("chart reader not wired")
	}
	page, err := reader.GetChartPage(ctx, canonicalUserID(user), req.Cursor)
	if errors.Is(err, domain.ErrCursorFormat) {
		return nil, invalid("INVALID_FORMAT")
	}
	if errors.Is(err, domain.ErrCursorRange) {
		return nil, invalid("OUT_OF_RANGE")
	}
	if err != nil || page == nil {
		return nil, finerr.WithRequestRetryInfo(finerr.InternalError(), true)
	}
	block, err := navigationChart(ctx, page)
	if err != nil {
		return nil, finerr.WithRequestRetryInfo(finerr.InternalError(), true)
	}
	return &analyticsv1.GetChartPageResponse{Block: block, TimeZone: page.TimeZone}, nil
}

func navigationChart(ctx context.Context, page *domain.ChartPage) (*commonv1.Block, error) {
	if page == nil {
		return unavailableBlock("monthly_chart"), nil
	}
	if page.TimeZone == "" || page.TimeZone == "Local" {
		return nil, domain.ErrSourceZone
	}
	zone, err := time.LoadLocation(page.TimeZone)
	if err != nil {
		return nil, err
	}
	m, err := metric(domain.Metric{Details: page.Details})
	if err != nil {
		return nil, err
	}
	points := m.GetMonthlyChart().GetPoints()
	if len(points) == 0 || len(points) > 60 || page.Revision == "" {
		return nil, errors.New("invalid chart page")
	}
	p := composition{zone: zone, s: domain.Analytics{AnalyticsData: domain.AnalyticsData{Chart: page}, Key: domain.SnapshotKey{PeriodKey: domain.PeriodKey{Type: domain.PeriodAllTime}}}}
	bars := []any{}
	alt := []string{}
	for _, point := range points {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if point.GetRange().GetStart() == nil || point.GetRange().GetEnd() == nil || (domain.PeriodKey{Type: domain.PeriodMonth, Key: point.Month}).Validate() != nil {
			return nil, errors.New("invalid monthly point")
		}
		start, end := point.Range.Start.AsTime(), point.Range.End.AsTime()
		if !profileCalendarMatches(domain.PeriodKey{Type: domain.PeriodMonth, Key: point.Month}, &domain.TimeRange{Start: &start, End: &end}, zone) {
			return nil, errors.New("chart point is not a full source-calendar month")
		}
		covered := point.GetBounds().GetCoverage()
		if (covered != commonv1.Coverage_COVERAGE_COMPLETE && covered != commonv1.Coverage_COVERAGE_PARTIAL && covered != commonv1.Coverage_COVERAGE_NO_DATA) ||
			(covered == commonv1.Coverage_COVERAGE_NO_DATA) != (point.ExpenseMinor == nil) {
			return nil, errors.New("chart point value disagrees with coverage")
		}
		var value any
		if point.ExpenseMinor != nil {
			value = point.GetExpenseMinor()
		}
		bar := p.barPoint(point, value)
		if action := monthSelection(point); action != nil {
			bar["action"] = map[string]any{"type": action.Type, "period": "month", "date": action.GetPeriodKey()}
		}
		bars = append(bars, bar)
		label := "нет данных"
		if point.ExpenseMinor != nil {
			label = money(point.GetExpenseMinor())
		}
		alt = append(alt, p.chartMonthName(point.Month, monthNom)+" "+label)
	}
	meta := map[string]any{"revision": page.Revision}
	if page.Previous != "" {
		meta["previous"] = page.Previous
	}
	if page.Next != "" {
		meta["next"] = page.Next
	}
	d, err := data(map[string]any{"kind": "bar", "value_kind": "amount", "points": bars, "page": meta})
	if err != nil {
		return nil, err
	}
	return &commonv1.Block{Id: "monthly_chart", Type: "chart", V: 1, Status: "ready", Title: ptr(title("monthly_chart")), AltText: ptr("Расходы по месяцам всей загруженной истории: " + strings.Join(alt, ", ")), HelpText: ptr(chartHelp), Data: d}, nil
}
