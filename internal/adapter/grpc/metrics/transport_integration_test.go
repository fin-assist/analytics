package metrics_test

// Integration tests of the handlers on a real gRPC transport (bufconn) over the production
// usecase, repository and tombstone library on PostgreSQL 18: the conversion and every rule of
// the batch as the producer's call sees it, and the user and system variants of ListPeriods.
// Identity and the access table are the server's (API tests in internal/app); here a test
// interceptor puts the verified user into the context the way go-grpc/server does.

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-migrate"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
	"github.com/fin-assist/analytics/internal/testdb"
	"github.com/fin-assist/analytics/migrations"
)

const (
	alice = "01a107db-ae80-7b07-b8a1-00000000a11c"
	bob   = "01a107db-ae80-7b07-b8a1-000000000b0b"
)

type noObserver struct{}

func (noObserver) IncompletePeriod(context.Context, domain.PeriodKey) {}

// asUser is the verified sub the server would put into the context; metadata "test-user".
func asUser(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if md, _ := metadata.FromIncomingContext(ctx); len(md.Get("test-user")) == 1 {
		ctx = reqctx.WithUserID(ctx, md.Get("test-user")[0])
	}
	return handler(ctx, req)
}

type transport struct {
	ingest    analyticsv1.MetricsIngestServiceClient
	analytics analyticsv1.AnalyticsServiceClient
	db        *postgres.DB
}

func startTransport(t *testing.T) transport {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(ctx, cfg, []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: dsn, MaxConns: 4, AppName: "transport-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	stones, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	uc, err := metrics.New(db, postgres.NewMetrics(db), stones, noObserver{})
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(asUser))
	screen, err := metrics.NewScreen(postgres.NewMetrics(db), func() time.Time { return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	composer, err := metricsgrpc.NewComposer(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	metricsgrpc.Register(s, uc, screen, composer, slog.New(slog.DiscardHandler))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///metrics",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return transport{analyticsv1.NewMetricsIngestServiceClient(conn), analyticsv1.NewAnalyticsServiceClient(conn), db}
}

func at(day int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC))
}

func span(from, to int) *commonv1.TimeRange { return &commonv1.TimeRange{Start: at(from), End: at(to)} }

func batch(user string, version int32) *analyticsv1.UpsertUserMetricsRequest {
	period := func(mode commonv1.TransferMode) *analyticsv1.PeriodSnapshot {
		return &analyticsv1.PeriodSnapshot{
			Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-09", TransferMode: mode, Range: span(1, 30),
			Bounds:       &commonv1.DataBounds{Coverage: commonv1.Coverage_COVERAGE_PARTIAL, DataFrom: at(1), DataTo: at(20), Gaps: []*commonv1.TimeRange{span(3, 4)}},
			CalculatedAt: at(21),
			Metrics: []*analyticsv1.MetricSnapshot{{MetricId: "tiles.expense", DefinitionVersion: 1, Metric: &analyticsv1.Metric{
				Status: analyticsv1.MetricStatus_METRIC_STATUS_READY, ValueMinor: proto.Int64(1), Range: span(1, 20),
				Lock:    &analyticsv1.MetricLock{Reason: analyticsv1.LockReason_LOCK_REASON_NEED_FULL_MONTHS},
				Basis:   &analyticsv1.MetricBasis{Range: span(1, 2), FullMonths: 1},
				Filters: &commonv1.OperationsFilter{From: at(1), To: at(30), Kind: commonv1.KindFilter_KIND_FILTER_EXPENSE},
				Details: &analyticsv1.Metric_BankFees{BankFees: &analyticsv1.BankFees{}},
			}}},
		}
	}
	return &analyticsv1.UpsertUserMetricsRequest{
		UserId: user, CalcVersion: version,
		State: &analyticsv1.UserDataState{HasData: true, DataFrom: at(1), DataTo: at(21), LastOperationAt: at(20), StaleAfter: at(28),
			FullMonths: 1, TimeZone: "Europe/Moscow", CalculatedAt: at(21)},
		Periods:        []*analyticsv1.PeriodSnapshot{period(commonv1.TransferMode_TRANSFER_MODE_WITH), period(commonv1.TransferMode_TRANSFER_MODE_WITHOUT)},
		RemovedPeriods: []*analyticsv1.PeriodRef{{Period: commonv1.PeriodType_PERIOD_TYPE_MONTH, PeriodKey: "2026-08"}},
	}
}

func bareInvalid(t *testing.T, err error) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument || len(st.Details()) != 0 || server.Reason(err) != "" {
		t.Fatalf("error %v, want a bare INVALID_ARGUMENT", err)
	}
}

func rows(t *testing.T, tr transport, user string) int {
	t.Helper()
	var n int
	if err := tr.db.Primary().QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM user_data_state WHERE user_id = $1) + (SELECT count(*) FROM period_snapshot WHERE user_id = $1)`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every rule of the batch, broken one at a time over the wire: a bare INVALID_ARGUMENT and
// nothing written; the valid batch is then applied whole.
func TestIntegrationUpsertUserMetricsRulesOverTransport(t *testing.T) {
	tr := startTransport(t)
	ctx := context.Background()
	first := func(r *analyticsv1.UpsertUserMetricsRequest) *analyticsv1.PeriodSnapshot { return r.Periods[0] }
	metric := func(r *analyticsv1.UpsertUserMetricsRequest) *analyticsv1.Metric {
		return r.Periods[0].Metrics[0].Metric
	}
	bad := timestamppb.Timestamp{Nanos: 2e9}
	cases := map[string]func(*analyticsv1.UpsertUserMetricsRequest){
		"user_id":              func(r *analyticsv1.UpsertUserMetricsRequest) { r.UserId = "u1" },
		"calc_version":         func(r *analyticsv1.UpsertUserMetricsRequest) { r.CalcVersion = -3 },
		"state time zone":      func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.TimeZone = "../UTC" },
		"state without moment": func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.CalculatedAt = nil },
		"state one bound":      func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.DataTo = nil },
		"state stale":          func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.StaleAfter = at(2) },
		"state has data":       func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.LastOperationAt = nil },
		"state full months":    func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.FullMonths = -1 },
		"state invalid moment": func(r *analyticsv1.UpsertUserMetricsRequest) { r.State.DataFrom = &bad },
		"period type":          func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Period = commonv1.PeriodType(8) },
		"period key":           func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).PeriodKey = "2026-13" },
		"transfer mode":        func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).TransferMode = 0 },
		"duplicate snapshot":   func(r *analyticsv1.UpsertUserMetricsRequest) { r.Periods[1].TransferMode = first(r).TransferMode },
		"sent and removed":     func(r *analyticsv1.UpsertUserMetricsRequest) { r.RemovedPeriods[0].PeriodKey = "2026-09" },
		"removed twice": func(r *analyticsv1.UpsertUserMetricsRequest) {
			r.RemovedPeriods = append(r.RemovedPeriods, r.RemovedPeriods[0])
		},
		"removed key":   func(r *analyticsv1.UpsertUserMetricsRequest) { r.RemovedPeriods[0].PeriodKey = "2026-8" },
		"range":         func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Range = span(30, 1) },
		"range invalid": func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Range.Start = &bad },
		"coverage": func(r *analyticsv1.UpsertUserMetricsRequest) {
			first(r).Bounds.Coverage = commonv1.Coverage_COVERAGE_NO_DATA
		},
		"data bounds":        func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Bounds.DataTo = nil },
		"data bound invalid": func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Bounds.DataTo = &bad },
		"calculated invalid": func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).CalculatedAt = &bad },
		"gap":                func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Bounds.Gaps[0].End = nil },
		"gap invalid":        func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Bounds.Gaps[0].End = &bad },
		"metric id":          func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Metrics[0].MetricId = "" },
		"metric twice": func(r *analyticsv1.UpsertUserMetricsRequest) {
			first(r).Metrics = append(first(r).Metrics, first(r).Metrics[0])
		},
		"metric without value": func(r *analyticsv1.UpsertUserMetricsRequest) { first(r).Metrics[0].Metric = nil },
		"status unspecified":   func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Status = 0 },
		"status unavailable": func(r *analyticsv1.UpsertUserMetricsRequest) {
			metric(r).Status = analyticsv1.MetricStatus_METRIC_STATUS_UNAVAILABLE
		},
		"status out of smallint": func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Status = 65537 },
		"metric range":           func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Range.End = nil },
		"metric range invalid":   func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Range.End = &bad },
		"lock reason":            func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Lock.Reason = 0 },
		"lock moment":            func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Lock.AvailableFrom = &bad },
		"basis range":            func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Basis.Range = span(2, 1) },
		"basis invalid":          func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Basis.Range.Start = &bad },
		"filter period":          func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.To = nil },
		"filter from invalid":    func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.From = &bad },
		"filter to invalid":      func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.To = &bad },
		"filter kind":            func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.Kind = 40000 },
		"filter amount":          func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.AmountToMinor = proto.Int64(5) },
		"filter q":               func(r *analyticsv1.UpsertUserMetricsRequest) { metric(r).Filters.Q = proto.String("x") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := batch(alice, 2)
			change(r)
			_, err := tr.ingest.UpsertUserMetrics(ctx, r)
			bareInvalid(t, err)
			if n := rows(t, tr, alice); n != 0 {
				t.Fatalf("the refused batch wrote %d rows", n)
			}
		})
	}
	resp, err := tr.ingest.UpsertUserMetrics(ctx, batch(alice, 2))
	if err != nil || resp.GetAppliedPeriods() != 2 || resp.GetSkippedStalePeriods() != 0 {
		t.Fatalf("valid batch: %v, %v", resp, err)
	}
	// state + two headers + two markers of 2026-08
	if n := rows(t, tr, alice); n != 5 {
		t.Fatalf("valid batch wrote %d rows, want 5", n)
	}
	resp, err = tr.ingest.UpsertUserMetrics(ctx, batch(alice, 1))
	if err != nil || resp.GetAppliedPeriods() != 0 || resp.GetSkippedStalePeriods() != 2 {
		t.Fatalf("stale batch: %v, %v", resp, err)
	}
}

func TestIntegrationListPeriodsOverTransport(t *testing.T) {
	tr := startTransport(t)
	ctx := context.Background()
	if _, err := tr.ingest.UpsertUserMetrics(ctx, batch(alice, 1)); err != nil {
		t.Fatal(err)
	}
	user := metadata.AppendToOutgoingContext(ctx, "test-user", alice)
	resp, err := tr.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{})
	if err != nil || len(resp.GetPeriods()) != 1 || resp.GetPeriods()[0].GetKey() != "2026-09" || resp.GetTimeZone() != "Europe/Moscow" ||
		!resp.GetPeriods()[0].GetRange().GetStart().AsTime().Equal(at(1).AsTime()) || resp.GetPeriods()[0].GetCoverage() != commonv1.Coverage_COVERAGE_PARTIAL {
		t.Fatalf("user periods %v, %v", resp, err)
	}
	// The system call names the user; another user's id never overrides the verified sub.
	resp, err = tr.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{UserId: proto.String(alice), Period: commonv1.PeriodType_PERIOD_TYPE_YEAR})
	if err != nil || len(resp.GetPeriods()) != 0 {
		t.Fatalf("system call for years: %v, %v", resp, err)
	}
	_, err = tr.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{UserId: proto.String(bob)})
	bareInvalid(t, err)
	_, err = tr.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{})
	bareInvalid(t, err)
	_, err = tr.analytics.ListPeriods(ctx, &analyticsv1.ListPeriodsRequest{UserId: proto.String("not-a-uuid")})
	bareInvalid(t, err)
	_, err = tr.analytics.ListPeriods(user, &analyticsv1.ListPeriodsRequest{Period: commonv1.PeriodType(5)})
	if server.Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("unknown period type: %v", err)
	}
	// A cancelled call reaches neither the repository nor the client as anything but its own code.
	cancelled, cancel := context.WithCancel(user)
	cancel()
	if _, err := tr.analytics.ListPeriods(cancelled, &analyticsv1.ListPeriodsRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled call: %v", err)
	}
}
