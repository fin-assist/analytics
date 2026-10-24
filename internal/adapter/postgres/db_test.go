package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/analytics/internal/reqctx"
)

func open(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := migratetest.DSN(t)
	db, err := postgres.Open(context.Background(), postgres.Options{PrimaryDSN: dsn, AppName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Writer(context.Background()).Exec(context.Background(), `CREATE TABLE item (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func count(t *testing.T, db *postgres.DB) int {
	t.Helper()
	var n int
	if err := db.Reader(context.Background()).QueryRow(context.Background(), `SELECT count(*) FROM item`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insert(ctx context.Context, db *postgres.DB, id int) error {
	_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO item (id) VALUES ($1)`, id)
	return err
}

func TestRunCommitsAndRollsBack(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if err := db.Run(ctx, func(ctx context.Context) error { return insert(ctx, db, 1) }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := db.Run(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, 2); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error %v, want boom", err)
	}
	if n := count(t, db); n != 1 {
		t.Fatalf("%d rows, want 1: the failed Run must roll back", n)
	}
}

func TestRunRollsBackOnPanic(t *testing.T) {
	db := open(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic must propagate")
			}
		}()
		_ = db.Run(context.Background(), func(ctx context.Context) error {
			_ = insert(ctx, db, 1)
			panic("handler bug")
		})
	}()
	if n := count(t, db); n != 0 {
		t.Fatalf("%d rows after panic, want 0", n)
	}
}

func TestNestedRunJoinsOuter(t *testing.T) {
	db := open(t)
	err := db.Run(context.Background(), func(ctx context.Context) error {
		if err := db.Run(ctx, func(ctx context.Context) error { return insert(ctx, db, 1) }); err != nil {
			return err
		}
		return errors.New("outer fails after inner succeeded")
	})
	if err == nil {
		t.Fatal("want the outer error")
	}
	if n := count(t, db); n != 0 {
		t.Fatalf("%d rows, want 0: the inner Run is part of the outer transaction", n)
	}
}

func TestTxIsVisibleOnlyInsideRun(t *testing.T) {
	db := open(t)
	if _, ok := db.Tx(context.Background()); ok {
		t.Fatal("no transaction outside Run")
	}
	_ = db.Run(context.Background(), func(ctx context.Context) error {
		tx, ok := db.Tx(ctx)
		if !ok {
			t.Fatal("transaction expected inside Run")
		}
		// The library writes through the same transaction as the effect.
		_, err := tx.Exec(ctx, `INSERT INTO item (id) VALUES (7)`)
		return err
	})
	if n := count(t, db); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestReaderUsesTransactionAndPrimaryHint(t *testing.T) {
	db := open(t)
	// Reader inside Run sees the uncommitted write of the same transaction.
	_ = db.Run(context.Background(), func(ctx context.Context) error {
		if err := insert(ctx, db, 1); err != nil {
			return err
		}
		var n int
		if err := db.Reader(ctx).QueryRow(ctx, `SELECT count(*) FROM item`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("reader in tx: %d, %v", n, err)
		}
		return nil
	})
	if q := db.Reader(reqctx.WithReadPrimary(context.Background())); q != db.Primary() {
		t.Fatal("x-read-primary must read from the primary pool")
	}
}

func TestOpenDoesNotLeakDSN(t *testing.T) {
	_, err := postgres.Open(context.Background(), postgres.Options{PrimaryDSN: "postgres://u:secret@%zz/db"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v must not contain the password", err)
	}
}

func TestIntegrationForeignTransactionDoesNotWriteOutsideItsOwner(t *testing.T) {
	owner, foreign := open(t), open(t)
	err := owner.Run(t.Context(), func(ctx context.Context) error {
		for name, q := range map[string]postgres.Querier{"Writer": foreign.Writer(ctx), "Reader": foreign.Reader(ctx)} {
			if _, err := q.Exec(ctx, `INSERT INTO item VALUES (9)`); !errors.Is(err, pgstore.ErrForeignTransaction) {
				return fmt.Errorf("%s: %w, want foreign transaction refusal", name, err)
			}
		}
		called := false
		err := foreign.Run(ctx, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, pgstore.ErrForeignTransaction) || called {
			return fmt.Errorf("foreign Run: %w, callback called=%v", err, called)
		}
		if _, ok := foreign.Tx(ctx); ok {
			return errors.New("foreign DB claims the owner's transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, owner) != 0 || count(t, foreign) != 0 {
		t.Fatal("foreign-context query persisted a write")
	}
}
