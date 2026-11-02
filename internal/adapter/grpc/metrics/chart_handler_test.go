package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/analytics/internal/domain"
)

type chartReaderFake struct {
	fakeScreen
	page         *domain.ChartPage
	chartErr     error
	ctx          context.Context
	user, cursor string
	calls        int
}

func (f *chartReaderFake) GetChartPage(ctx context.Context, user, cursor string) (*domain.ChartPage, error) {
	f.ctx, f.user, f.cursor = ctx, user, cursor
	f.calls++
	return f.page, f.chartErr
}

func TestUnitGetChartPageHandlerUsesVerifiedUserAndComposesPage(t *testing.T) {
	point := navigationPoint(t, "Europe/Moscow", "2026-10", commonv1.Coverage_COVERAGE_COMPLETE, ptr(int64(12500)))
	f := &chartReaderFake{page: navigationFixture(t, "Europe/Moscow", point)}
	h := &analytics{screen: f}
	ctx := reqctx.WithUserID(t.Context(), strings.ToUpper(user))
	got, err := h.GetChartPage(ctx, &analyticsv1.GetChartPageRequest{Cursor: "opaque-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || f.ctx != ctx || f.user != user || f.cursor != "opaque-cursor" || f.fakeScreen.user != "" {
		t.Fatalf("chart reader call=%+v; expected canonical verified identity and unchanged cursor", f)
	}
	if got.TimeZone != "Europe/Moscow" || got.Block.Id != "monthly_chart" || got.Block.Status != "ready" || got.Block.Type != "chart" {
		t.Fatalf("response=%+v", got)
	}
	d := valueOf(got.Block)
	points := d["points"].([]any)
	bar := points[0].(map[string]any)
	if len(points) != 1 || bar["value"] != float64(12500) || bar["action"].(map[string]any)["date"] != "2026-10" {
		t.Fatal(points)
	}
	page := d["page"].(map[string]any)
	if page["revision"] != "source-revision" || page["previous"] != "previous" || page["next"] != "next" {
		t.Fatal(page)
	}
}

func TestUnitGetChartPageHandlerMapsCursorValidation(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, reason string
		err                  error
		calls                int
	}{
		{"required", "", "REQUIRED", nil, 0},
		{"format", "foreign", "INVALID_FORMAT", fmt.Errorf("read: %w", domain.ErrCursorFormat), 1},
		{"range", "stale", "OUT_OF_RANGE", fmt.Errorf("read: %w", domain.ErrCursorRange), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &chartReaderFake{chartErr: tc.err}
			h := &analytics{screen: f}
			got, err := h.GetChartPage(reqctx.WithUserID(t.Context(), user), &analyticsv1.GetChartPageRequest{Cursor: tc.cursor})
			rest, ok := finerr.ToREST(err)
			if got != nil || status.Code(err) != codes.InvalidArgument || !ok || rest.Code != "VALIDATION_ERROR" || rest.Spec.HTTPStatus != http.StatusBadRequest || len(rest.Violations) != 1 || rest.Violations[0].Field != "cursor" || rest.Violations[0].Reason != tc.reason {
				t.Fatalf("response=%+v err=%v REST=%+v", got, err, rest)
			}
			if f.calls != tc.calls || (f.calls > 0 && (f.user != user || f.cursor != tc.cursor)) {
				t.Fatalf("reader call=%+v", f)
			}
		})
	}
	f := &chartReaderFake{}
	h := &analytics{screen: f}
	if got, err := h.GetChartPage(t.Context(), &analyticsv1.GetChartPageRequest{Cursor: "opaque-cursor"}); got != nil || !bareInvalid(err) || f.calls != 0 {
		t.Fatalf("missing verified user: response=%+v err=%v calls=%d", got, err, f.calls)
	}
}

func TestUnitGetChartPageHandlerInternalErrorsExplicitlyAllowRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		page *domain.ChartPage
		err  error
	}{
		{"store", nil, errors.New("private storage failure")},
		{"absent page", nil, nil},
		{"invalid page", &domain.ChartPage{TimeZone: "Local"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &chartReaderFake{page: tc.page, chartErr: tc.err}
			h := &analytics{screen: f}
			got, err := h.GetChartPage(reqctx.WithUserID(t.Context(), user), &analyticsv1.GetChartPageRequest{Cursor: "opaque-cursor"})
			rest, ok := finerr.ToREST(err)
			if got != nil || status.Code(err) != codes.Internal || !ok || rest.Code != "INTERNAL_ERROR" || rest.Spec.HTTPStatus != http.StatusInternalServerError || rest.RequestRetryable == nil || !*rest.RequestRetryable || len(rest.Violations) != 0 {
				t.Fatalf("response=%+v err=%v REST=%+v", got, err, rest)
			}
			if strings.Contains(err.Error(), "private storage") || f.calls != 1 {
				t.Fatalf("private error leaked or reader was not called: err=%v calls=%d", err, f.calls)
			}
		})
	}
}
