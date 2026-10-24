package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
)

// Test-only domain sentinel; analytics has no registered business RPC or mapper yet.
var errExampleCategoryNotFound = errors.New("category not found")

// This operation runner belongs to the PostgreSQL adapter, never to the usecase.
type assignCategoryTx struct{ db *postgres.DB }

func (r assignCategoryTx) Run(ctx context.Context, fn func(context.Context) error) error {
	return r.db.RunMapped(ctx, pgx.TxOptions{}, func(err error) error {
		if pgstore.IsViolation(err, "23503", "example_category_fk") {
			return errExampleCategoryNotFound
		}
		return nil
	}, fn)
}

// These consumer-side interfaces and the usecase belong to internal/<feature>.
type exampleTransactions interface {
	Run(context.Context, func(context.Context) error) error
}
type exampleCategories interface {
	Assign(context.Context, int) error
}
type assignCategory struct {
	transactions exampleTransactions
	categories   exampleCategories
}

func (u assignCategory) Execute(ctx context.Context, id int) error {
	err := u.transactions.Run(ctx, func(ctx context.Context) error { return u.categories.Assign(ctx, id) })
	if errors.Is(err, errExampleCategoryNotFound) {
		return finerr.NotFound()
	}
	return err
}

// The repository propagates statement errors. RunMapped also catches deferred errors.
type exampleCategoryRepository struct{ db *postgres.DB }

func (r exampleCategoryRepository) Assign(ctx context.Context, id int) error {
	_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO example_assignment VALUES (1,$1)`, id)
	return err
}

func TestIntegrationAdapterMapsCommitAndUsecaseReturnsContractError(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	_, err := db.Writer(ctx).Exec(ctx, `CREATE TABLE example_category(id int PRIMARY KEY);
 CREATE TABLE example_assignment(id int PRIMARY KEY,category_id int,
 CONSTRAINT example_category_fk FOREIGN KEY(category_id) REFERENCES example_category(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	usecase := assignCategory{assignCategoryTx{db}, exampleCategoryRepository{db}}
	err = usecase.Execute(ctx, 987654)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("contract error: %v", err)
	}
	found := false
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "NOT_FOUND" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NOT_FOUND reason missing: %v", err)
	}
	var n int
	if err := db.Reader(ctx).QueryRow(ctx, `SELECT count(*) FROM example_assignment`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed commit persisted assignment: %d %v", n, err)
	}
}

func TestIntegrationRunMappedPreservesUnknownViolation(t *testing.T) {
	db := open(t)
	mapperCalled := false
	err := db.RunMapped(t.Context(), pgx.TxOptions{}, func(error) error {
		mapperCalled = true
		return nil
	}, func(ctx context.Context) error {
		if err := insert(ctx, db, 1); err != nil {
			return err
		}
		return insert(ctx, db, 1)
	})
	if !mapperCalled || !pgstore.IsViolation(err, "23505", "item_pkey") {
		t.Fatalf("unknown violation must remain intact: %v, mapper called=%v", err, mapperCalled)
	}
	if count(t, db) != 0 {
		t.Fatal("unknown violation committed the partial operation")
	}
}

func TestIntegrationDeferredReferenceCanBeRepairedBeforeCommit(t *testing.T) {
	db := open(t)
	_, err := db.Writer(t.Context()).Exec(t.Context(), `CREATE TABLE test_parent(id int PRIMARY KEY);
 CREATE TABLE test_child(id int PRIMARY KEY,parent_id int,
 CONSTRAINT test_parent_fk FOREIGN KEY(parent_id) REFERENCES test_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	err = db.RunMapped(t.Context(), pgx.TxOptions{}, func(err error) error {
		if pgstore.IsViolation(err, "23503", "test_parent_fk") {
			return errExampleCategoryNotFound
		}
		return nil
	}, func(ctx context.Context) error {
		if _, err := db.Writer(ctx).Exec(ctx, `INSERT INTO test_child VALUES (1,7)`); err != nil {
			return err
		}
		return db.Run(ctx, func(ctx context.Context) error {
			_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO test_parent VALUES (7)`)
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Reader(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM test_child`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("repaired reference did not commit: %d, %v", n, err)
	}
}

func TestIntegrationRunWithPreservesOuterOptionsAndRejectsNestedMapper(t *testing.T) {
	db := open(t)
	outer := pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}
	err := db.RunWith(t.Context(), outer, func(ctx context.Context) error {
		called := false
		err := db.RunMapped(ctx, pgx.TxOptions{}, func(error) error { return nil }, func(context.Context) error {
			called = true
			return nil
		})
		if !errors.Is(err, pgstore.ErrNestedMapper) || called {
			return errors.New("nested mapper was not rejected before callback")
		}
		return db.RunWith(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(ctx context.Context) error {
			var isolation, readOnly string
			if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
				return err
			}
			if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
				return err
			}
			if isolation != "serializable" || readOnly != "on" {
				return errors.New("nested RunWith changed the outer transaction options")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
