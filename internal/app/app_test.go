package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/config"
)

// Outside dev the service refuses to start until real verifiers are wired (FIN-119),
// instead of serving with identities nobody checks.
func TestUnitIdentityFailsClosed(t *testing.T) {
	_, _, _, err := identity(config.Config{Service: "user", Env: config.EnvProd, Auth: config.AuthConfig{Mode: config.AuthVerify}})
	if err == nil || !strings.Contains(err.Error(), "FIN-119") {
		t.Fatalf("error %v", err)
	}
	s, u, tok, err := identity(config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}})
	if err != nil || s == nil || u == nil || tok == nil {
		t.Fatalf("dev: %v", err)
	}
}

// The dev-only gate holds for callers of New that bypass config.Load (apitest, a
// hand-built Config): unverified identities are refused before any pool is opened.
func TestUnitInsecureDevRefusedOutsideDevWithoutConfigLoad(t *testing.T) {
	for _, env := range []config.Env{config.EnvStage, config.EnvProd, ""} {
		t.Run(string(env), func(t *testing.T) {
			cfg := config.Config{Service: "analytics", Env: env, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
				DB: config.DBConfig{PrimaryDSN: "primary", MaxConns: 1}}
			s, u, tok, err := identity(cfg)
			if err == nil || s != nil || u != nil || tok != nil || !strings.Contains(err.Error(), "only with ENV=dev") {
				t.Fatalf("insecure identities accepted in %q: %v", env, err)
			}
			opened := false
			a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
				opened = true
				return nil, errors.New("must not open")
			})
			if a != nil || err == nil || opened {
				t.Fatalf("assembly in %q: app=%v opened=%v err=%v", env, a != nil, opened, err)
			}
		})
	}
}

func TestIntegrationNewFailsOnUnreachableDatabase(t *testing.T) {
	cfg := config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB: config.DBConfig{PrimaryDSN: "postgres://nobody@127.0.0.1:1/db?connect_timeout=1", MaxConns: 1}}
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("want an error")
	}
}
