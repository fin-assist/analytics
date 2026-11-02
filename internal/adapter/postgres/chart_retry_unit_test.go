package postgres

import (
	"context"
	"errors"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnitChartSnapshotRetriesOnlyOwnedSerializationFailures(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failures, want int
		err            error
	}{
		{"initial collision", 1, 2, &pgconn.PgError{Code: "40001"}},
		{"bounded contention", 9, 3, &pgconn.PgError{Code: "40001"}},
		{"other SQL failure", 1, 1, &pgconn.PgError{Code: "23503"}},
		{"ordinary failure", 1, 1, errors.New("storage")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beginner := &recordingBeginner{tx: unitTransaction{}}
			repo := NewMetrics(&DB{transactions: pgstore.New(beginner)}, nil)
			calls := 0
			err := repo.chartSnapshot(t.Context(), func(context.Context) error {
				calls++
				if calls <= tc.failures {
					return tc.err
				}
				return nil
			})
			if calls != tc.want || beginner.opts.IsoLevel != pgx.RepeatableRead || (calls > tc.failures && err != nil) || (calls <= tc.failures && !errors.Is(err, tc.err)) {
				t.Fatal(calls, err, beginner.opts)
			}
		})
	}
	beginner := &recordingBeginner{tx: unitTransaction{}}
	db := &DB{transactions: pgstore.New(beginner)}
	repo := NewMetrics(db, nil)
	boom := &pgconn.PgError{Code: "40001"}
	calls := 0
	err := db.Run(t.Context(), func(ctx context.Context) error {
		return repo.chartSnapshot(ctx, func(context.Context) error { calls++; return boom })
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatal("retried outer transaction", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = repo.chartSnapshot(ctx, func(context.Context) error { t.Fatal("ran cancelled read"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
