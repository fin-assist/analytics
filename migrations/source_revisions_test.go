package migrations_test

import (
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fin-assist/go-migrate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/analytics/internal/testdb"
	"github.com/fin-assist/analytics/migrations"
)

func TestIntegrationProvenanceUpgradeDoesNotInventLegacyBaselines(t *testing.T) {
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "000002" {
			data, err := fs.ReadFile(migrations.FS, entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			legacy[entry.Name()] = &fstest.MapFile{Data: data}
		}
	}
	dsn := testdb.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.DiscardHandler)
	source := migrate.Source{Name: "service", Table: "schema_migrations", FS: legacy}
	if err = migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	})
	user := uuid.NewString()
	if _, err = conn.Exec(t.Context(), `INSERT INTO user_data_state(user_id,calc_version,has_data,full_months,recalculating,time_zone,calculated_at)VALUES($1,1,false,0,false,'Europe/Moscow','2026-10-01')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `INSERT INTO period_snapshot(user_id,period_type,period_key,transfer_mode,calc_version,removed,range_start,range_end,coverage,data_from,data_to,unread_lines_count,calculated_at)
 VALUES($1,1,'2026-09',1,1,false,'2026-09-01','2026-10-01',1,'2026-09-01','2026-09-30',0,'2026-10-01')`, user); err != nil {
		t.Fatal(err)
	}
	var stateBefore, headerBefore string
	if err = conn.QueryRow(t.Context(), `SELECT to_jsonb(s)::text FROM user_data_state s WHERE user_id=$1`, user).Scan(&stateBefore); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(t.Context(), `SELECT to_jsonb(s)::text FROM period_snapshot s WHERE user_id=$1`, user).Scan(&headerBefore); err != nil {
		t.Fatal(err)
	}
	source.FS = migrations.FS
	if err = migrate.Up(t.Context(), cfg, []migrate.Source{source}); err != nil {
		t.Fatal(err)
	}
	verify := func() {
		t.Helper()
		var stateAfter, headerAfter string
		var stateUnknown, headerUnknown bool
		if err = conn.QueryRow(t.Context(), `SELECT (to_jsonb(s)-'data_revisions')::text,data_revisions IS NULL FROM user_data_state s WHERE user_id=$1`, user).Scan(&stateAfter, &stateUnknown); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRow(t.Context(), `SELECT (to_jsonb(s)-'data_revisions')::text,data_revisions IS NULL FROM period_snapshot s WHERE user_id=$1`, user).Scan(&headerAfter, &headerUnknown); err != nil {
			t.Fatal(err)
		}
		if !stateUnknown || !headerUnknown || stateBefore != stateAfter || headerBefore != headerAfter {
			t.Fatal("upgrade invented provenance or changed financial facts")
		}
	}
	verify()
	for _, name := range []string{"000002_source_revisions.down.sql", "000002_source_revisions.up.sql"} {
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Exec(t.Context(), string(data)); err != nil {
			t.Fatal(err)
		}
	}
	verify()
}
