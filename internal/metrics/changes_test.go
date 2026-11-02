package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
)

type markerStore struct {
	versions []int32
	err      error
}

func (s *markerStore) MarkRecalculating(_ context.Context, _ string, v int32) error {
	s.versions = append(s.versions, v)
	return s.err
}

func TestUnitChangesRequireValidatedIdentityAndDependencies(t *testing.T) {
	tx, g, s := &fakeTx{}, &fakeGuard{}, &markerStore{}
	for _, deps := range []struct {
		tx Transactions
		g  Guard
		s  ChangeStore
	}{{nil, g, s}, {tx, nil, s}, {tx, g, nil}} {
		if _, err := NewChanges(deps.tx, deps.g, deps.s); err == nil {
			t.Fatal("missing dependency accepted")
		}
	}
	c, err := NewChanges(tx, g, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int32{-1, 0} {
		if err = c.Mark(t.Context(), user, v); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if err = c.Mark(t.Context(), "not-user", 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if err = c.Apply(t.Context(), user, 1, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if len(s.versions) > 0 {
		t.Fatal("invalid requests caused effect")
	}
}

func TestUnitChangesRollbackAndDeletedUserOutcomes(t *testing.T) {
	boom := errors.New("fail")
	for _, stage := range []string{"tx", "guard", "store", "compose", "deleted"} {
		t.Run(stage, func(t *testing.T) {
			tx, g, s := &fakeTx{}, &fakeGuard{}, &markerStore{}
			c, _ := NewChanges(tx, g, s)
			called := false
			switch stage {
			case "tx":
				tx.err = boom
			case "guard":
				g.err = boom
			case "store":
				s.err = boom
			case "deleted":
				g.deleted = true
			}
			err := c.Apply(t.Context(), user, 8, func(context.Context) error { called = true; return boom })
			if stage == "deleted" {
				if !errors.Is(err, ErrDeleted) {
					t.Fatal(err)
				}
				if err = c.Mark(t.Context(), user, 9); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, boom) {
				t.Fatal(err)
			}
			if (stage == "compose") != called {
				t.Fatal("composition after failed admission", stage)
			}
			if tx.committed != 0 && stage != "deleted" {
				t.Fatal("failure committed")
			}
		})
	}
	tx, g, s := &fakeTx{}, &fakeGuard{}, &markerStore{}
	c, _ := NewChanges(tx, g, s)
	if err := c.Apply(t.Context(), user, 9, func(context.Context) error { return nil }); err != nil || tx.committed != 1 {
		t.Fatal(err)
	}
}
