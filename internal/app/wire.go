package app

import (
	"context"
	"errors"
	"log/slog"

	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-migrate"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-worker"

	metricsgrpc "github.com/fin-assist/analytics/internal/adapter/grpc/metrics"
	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/domain"
	"github.com/fin-assist/analytics/internal/metrics"
	"github.com/fin-assist/analytics/migrations"
)

// AccessPolicy is the table «method → callers → model» of this service, copied from
// grpc-api.md 3.3 (the source of truth). The server refuses to start while a registered
// method has no rule. AnalyticsService is mixed: bank-bff calls with the user's token,
// assistant without one and names the user in user_id.
func AccessPolicy() server.Policy {
	return server.Policy{
		"/finassist.analytics.v1.AnalyticsService/*":       {Callers: []string{"bank-bff", "assistant"}, Model: server.ModelMixed},
		"/finassist.analytics.v1.MetricsIngestService/*":   {Callers: []string{"bank-statement-data"}, Model: server.ModelSystem},
		"/finassist.userdeletion.v1.UserDeletionService/*": {Callers: []string{"user"}, Model: server.ModelSystem},
	}
}

// MigrationSources lists the migration sources in the order they are applied: libraries
// first (each with its own version table), then the service.
func MigrationSources() []migrate.Source {
	return []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire builds repositories, usecases and handlers, registers the handlers on a.GRPC.GRPC()
// and returns the background jobs and a function that closes what it opened. The order is
// adapters → usecases → delivery.
func wire(a *App) (jobs []worker.Job, closeAll func(), err error) {
	repo := postgres.NewMetrics(a.DB)
	incomplete, err := newIncompletePeriods(a.Registry, a.Log)
	if err != nil {
		return nil, nil, err
	}
	uc, err := metrics.New(a.DB, repo, a.Tombstones, incomplete)
	if err != nil {
		return nil, nil, err
	}
	metricsgrpc.Register(a.GRPC.GRPC(), uc, a.Log)
	deletions, err := tombstone.NewDeletionServer(transactor{a.DB}, a.Tombstones, repo.Purge)
	if err != nil {
		return nil, nil, err
	}
	userdeletionv1.RegisterUserDeletionServiceServer(a.GRPC.GRPC(), deletions)
	return nil, func() {}, nil
}

// transactor is the go-tombstone Transactor over postgres.DB: one READ COMMITTED transaction
// of the primary, rolled back on any error, committed before Within returns nil.
type transactor struct{ db *postgres.DB }

func (t transactor) Within(ctx context.Context, fn func(context.Context) error) error {
	return t.db.Run(ctx, fn)
}

// incompletePeriods is the Observer of periods sent in one transfer mode (grpc-api.md 7.3, 9):
// a warning without user data and finassist_incomplete_period_snapshots_total{period_type}.
type incompletePeriods struct {
	counter *prometheus.CounterVec
	log     *slog.Logger
}

func newIncompletePeriods(reg prometheus.Registerer, log *slog.Logger) (incompletePeriods, error) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "finassist_incomplete_period_snapshots_total",
		Help: "Periods UpsertUserMetrics applied with a snapshot of one transfer mode only.",
	}, []string{"period_type"})
	if reg == nil {
		return incompletePeriods{}, errors.New("metrics registry required")
	}
	if err := reg.Register(counter); err != nil {
		return incompletePeriods{}, err
	}
	return incompletePeriods{counter: counter, log: log}, nil
}

func (o incompletePeriods) IncompletePeriod(ctx context.Context, period domain.PeriodKey) {
	o.counter.WithLabelValues(period.Type.String()).Inc()
	o.log.WarnContext(ctx, "period snapshot in one transfer mode", "period_type", period.Type.String(), "period_key", period.Key)
}
