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

// The dev-only gate holds for callers of New that bypass config.Load (apitest, a
// hand-built Config): unverified identities are refused before any pool is opened.
func TestUnitInsecureDevRefusedOutsideDevWithoutConfigLoad(t *testing.T) {
	for _, env := range []config.Env{config.EnvStage, config.EnvProd, ""} {
		t.Run(string(env), func(t *testing.T) {
			cfg := config.Config{Service: "analytics", Env: env, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
				DB: config.DBConfig{PrimaryDSN: "primary", MaxConns: 1}}
			id, err := identity(cfg, prometheus.NewRegistry(), nil)
			if err == nil || id.Services != nil || id.Users != nil || id.Tokens != nil || !strings.Contains(err.Error(), "requires ENV=dev") {
				t.Fatalf("insecure identities accepted in %q: %v", env, err)
			}
			opened := false
			a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
				opened = true
				return nil, errors.New("must not open")
			}, nil)
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
