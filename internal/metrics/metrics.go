// Package metrics holds the usecases of the stored personal metrics (FIN-283): applying a
// batch of UpsertUserMetrics, listing the periods with data and deleting a user's data. It
// owns the transaction boundary and the order of its statements; the repository, the
// tombstone guard and the observer of incomplete periods are interfaces it declares.
package metrics

import (
	"context"
	"errors"
	"fmt"

	"github.com/fin-assist/analytics/internal/domain"
)

// Transactions runs fn in one READ COMMITTED transaction of the primary and commits it
// when fn returns nil (postgres.DB.Run).
type Transactions interface {
	Run(ctx context.Context, fn func(context.Context) error) error
}

// Guard is the tombstone check inside a writing transaction (go-tombstone Store.Guard): it
// serializes the writes and the deletion of one user and returns false for a deleted user.
type Guard interface {
	Guard(ctx context.Context, userID string) (alive bool, err error)
}

// Store is the repository of the metrics. Writes run inside Transactions.Run.
type Store interface {
	UpsertState(ctx context.Context, userID string, version int32, s domain.State) error
	UpsertPeriods(ctx context.Context, userID string, version int32, snapshots []domain.Snapshot, removed []domain.PeriodKey) (applied int, err error)
	ListPeriods(ctx context.Context, userID string, typ domain.PeriodType) (domain.Periods, error)
}

// Observer hears of the periods a committed batch carried in one transfer mode only.
// grpc-api.md 7.3: they are applied as they came, with a warning and the metric
// finassist_incomplete_period_snapshots_total.
type Observer interface {
	IncompletePeriod(ctx context.Context, period domain.PeriodKey)
}

// Result is what a batch applied: snapshots written and snapshots skipped as stale.
type Result struct {
	Applied, Skipped int
}

// Metrics is the usecase. It is safe for concurrent use when its dependencies are.
type Metrics struct {
	tx       Transactions
	store    Store
	guard    Guard
	observer Observer
}

// New wires the usecase; every dependency is required.
func New(tx Transactions, store Store, guard Guard, observer Observer) (*Metrics, error) {
	if tx == nil || store == nil || guard == nil || observer == nil {
		return nil, errors.New("metrics: dependencies required")
	}
	return &Metrics{tx: tx, store: store, guard: guard, observer: observer}, nil
}

// Upsert applies one UpsertUserMetrics batch atomically. A batch that breaks a rule of
// domain.Upsert.Validate returns an error wrapping domain.ErrInvalid and writes nothing.
// For a deleted user it returns a zero Result and nil: the late event is acknowledged
// without effects, so no data comes back after PurgeUserData. Other errors are those of
// the guard and the repository; the transaction is rolled back.
//
// Order in the transaction (docs/schema.md, «Запись»): the tombstone guard, which also
// serializes every write of the user; the state; the headers and markers; the gaps and
// metrics of the applied keys.
func (m *Metrics) Upsert(ctx context.Context, u domain.Upsert) (Result, error) {
	if err := u.Validate(); err != nil {
		return Result{}, err
	}
	var applied int
	deleted := false
	err := m.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := m.guard.Guard(ctx, u.UserID)
		if err != nil {
			return fmt.Errorf("tombstone guard: %w", err)
		}
		if !alive {
			deleted = true
			return nil
		}
		if u.State != nil {
			if err := m.store.UpsertState(ctx, u.UserID, u.Version, *u.State); err != nil {
				return fmt.Errorf("state: %w", err)
			}
		}
		if applied, err = m.store.UpsertPeriods(ctx, u.UserID, u.Version, u.Snapshots, u.Removed); err != nil {
			return fmt.Errorf("periods: %w", err)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if deleted {
		return Result{}, nil
	}
	// After the commit: a rolled back batch reports nothing, a redelivered one reports again.
	for _, period := range u.IncompletePeriods() {
		m.observer.IncompletePeriod(ctx, period)
	}
	return Result{Applied: applied, Skipped: len(u.Snapshots) - applied}, nil
}

// Periods lists the periods of typ with data for userID, newest first. userID must be
// verified by the caller (the token's sub or the user_id of an allowed system call); an
// invalid one or an unknown type returns an error wrapping domain.ErrInvalid.
func (m *Metrics) Periods(ctx context.Context, userID string, typ domain.PeriodType) (domain.Periods, error) {
	if !domain.ValidUserID(userID) {
		return domain.Periods{}, fmt.Errorf("%w: user_id is not a UUID", domain.ErrInvalid)
	}
	if !typ.Known() {
		return domain.Periods{}, fmt.Errorf("%w: period type %d", domain.ErrInvalid, typ)
	}
	return m.store.ListPeriods(ctx, userID, typ)
}
