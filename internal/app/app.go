// Package app assembles the service: identity, database pools, the gRPC server with its
// handlers, clients of other services and background jobs. cmd/analytics runs it with real
// listeners; apitest runs the same assembly in-process for API tests, so what the tests
// call is what production serves.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/config"
)

// App is the assembled service. Wire reads its fields to build adapters and usecases.
type App struct {
	Cfg        config.Config
	Log        *slog.Logger
	DB         *postgres.DB
	GRPC       *server.Server
	Clients    *client.Metrics
	Tokens     client.TokenSource
	Tombstones *tombstone.Store
	Registry   prometheus.Registerer
	Now        func() time.Time
	Jobs       []worker.Job

	closers []func()
}

// New assembles the service from cfg. Close releases what it opened, after the jobs
// (identity-keys among them) have stopped.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer) (*App, error) {
	return assemble(ctx, cfg, log, reg, postgres.Open, nil)
}

// assemble builds the service. userDial are extra options of the identity connection to
// user (in-process tests only).
func assemble(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer,
	openDB func(context.Context, postgres.Options) (*postgres.DB, error), userDial []grpc.DialOption,
) (*App, error) {
	id, err := identity(cfg, reg, userDial)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, postgres.Options{
		PrimaryDSN: cfg.DB.PrimaryDSN, ReplicaDSN: cfg.DB.ReplicaDSN,
		MaxConns: cfg.DB.MaxConns, AppName: cfg.Service,
	})
	if err != nil {
		id.Close()
		return nil, err
	}
	// Close runs in reverse order: the pools first, the identity user connection last.
	a := &App{Cfg: cfg, Log: log, DB: db, Tokens: id.Tokens, Registry: reg, Now: time.Now,
		closers: []func(){id.Close, db.Close}}
	fail := func(err error) (*App, error) { a.Close(); return nil, err }

	revoked, err := tombstone.NewRevokedCounter(reg)
	if err != nil {
		return fail(err)
	}
	// The tombstone of a deleted user is read on the primary, never a replica that lags.
	if a.Tombstones, err = tombstone.New(db, db.Primary(), a.Now); err != nil {
		return fail(err)
	}
	if a.GRPC, err = server.New(server.Options{MaxRecvMsgSize: cfg.GRPC.MaxRecvMsgSize, Reflection: cfg.GRPC.Reflection}, server.Deps{
		Logger: log, Metrics: reg, Policy: AccessPolicy(),
		Services: id.Services, Users: id.Users, Tombstones: tombstoneAdmission{a.Tombstones, revoked},
	}); err != nil {
		return fail(err)
	}
	if a.Clients, err = client.NewMetrics(reg); err != nil {
		return fail(err)
	}
	jobs, closeWired, err := wire(a)
	if err != nil {
		return fail(err)
	}
	if id.Job != nil {
		jobs = append(jobs, *id.Job)
	}
	a.Jobs = jobs
	a.closers = append(a.closers, closeWired)
	return a, nil
}

// Close releases what New opened, in reverse order.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// tombstoneAdmission is the server admission check of user calls: a deleted user gets
// TOKEN_REVOKED on any call (grpc-api.md 7.1); the counter counts those refusals.
type tombstoneAdmission struct {
	store   *tombstone.Store
	revoked prometheus.Counter
}

func (a tombstoneAdmission) IsDeleted(ctx context.Context, id string) (bool, error) {
	deleted, err := a.store.Deleted(ctx, id)
	if err == nil && deleted {
		a.revoked.Inc()
	}
	return deleted, err
}
