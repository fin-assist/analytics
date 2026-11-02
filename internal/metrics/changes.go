package metrics

import (
	"context"
	"errors"

	"github.com/fin-assist/analytics/internal/domain"
)

// ChangeStore writes only the monotonic true-delivery marker.
type ChangeStore interface {
	MarkRecalculating(context.Context, string, int32) error
}

// Changes serializes marker delivery and composition with ingestion and user deletion.
type Changes struct {
	tx    Transactions
	guard Guard
	store ChangeStore
}

// NewChanges requires the local transaction, tombstone guard and marker store.
func NewChanges(tx Transactions, guard Guard, store ChangeStore) (*Changes, error) {
	if tx == nil || guard == nil || store == nil {
		return nil, errors.New("changes: dependencies required")
	}
	return &Changes{tx: tx, guard: guard, store: store}, nil
}

// ErrDeleted is an authenticated user's mutation racing with account deletion.
var ErrDeleted = errors.New("user deleted")

// Mark acknowledges deleted users without resurrecting their state.
func (c *Changes) Mark(ctx context.Context, user string, version int32) error {
	return c.change(ctx, user, version, nil)
}

// Apply commits the marker only if local supplied-payload composition succeeds.
// The callback must perform no external I/O and must not mutate financial storage.
func (c *Changes) Apply(ctx context.Context, user string, version int32, compose func(context.Context) error) error {
	if compose == nil {
		return domain.ErrInvalid
	}
	return c.change(ctx, user, version, compose)
}

func (c *Changes) change(ctx context.Context, user string, version int32, compose func(context.Context) error) error {
	if !domain.ValidUserID(user) || version <= 0 {
		return domain.ErrInvalid
	}
	return c.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := c.guard.Guard(ctx, user)
		if err != nil {
			return err
		}
		if !alive {
			if compose != nil {
				return ErrDeleted
			}
			return nil
		}
		if err = c.store.MarkRecalculating(ctx, user, version); err != nil {
			return err
		}
		if compose != nil {
			return compose(ctx)
		}
		return nil
	})
}
