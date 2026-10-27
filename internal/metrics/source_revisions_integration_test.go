package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"testing"
	"time"

	"github.com/fin-assist/analytics/internal/adapter/postgres"
	"github.com/fin-assist/analytics/internal/domain"
	metricservice "github.com/fin-assist/analytics/internal/metrics"
)

// These tests join the domain usecase, transaction manager, real tombstone guard
// and PostgreSQL repository directly. They do not assemble an app or call gRPC.
func revisionBatch(user string, version int32, key string, revisions domain.Revisions) domain.Upsert {
	u := domain.Upsert{UserID: user, Version: version, State: state(version)}
	u.State.Revisions = maps.Clone(revisions)
	if key != "" {
		for _, mode := range domain.TransferModes {
			s := monthSnapshot(key, mode, version, "tiles.expense")
			s.Revisions = maps.Clone(revisions)
			u.Snapshots = append(u.Snapshots, s)
		}
	}
	return u
}

func applyRevisionBatch(t *testing.T, e *env, u domain.Upsert) {
	t.Helper()
	if _, err := e.uc.Upsert(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

func wantRevisionMap(t *testing.T, got, want domain.Revisions) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Fatalf("stored revisions %#v, want %#v", got, want)
	}
}

func checkRevisionRead(t *testing.T, e *env, user, key string, stateRevisions, headerRevisions domain.Revisions, hasHeader bool) {
	t.Helper()
	screen, err := metricservice.NewScreen(e.repo, func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, msk) })
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range domain.TransferModes {
		data, err := screen.Get(t.Context(), user, domain.PeriodMonth, &key, mode)
		if err != nil || data.State == nil || (data.Snapshot != nil) != hasHeader {
			t.Fatal("repository read", data, err)
		}
		wantRevisionMap(t, data.State.Revisions, stateRevisions)
		if data.Snapshot != nil {
			wantRevisionMap(t, data.Snapshot.Revisions, headerRevisions)
		}
	}
}

func checkRevisionList(t *testing.T, e *env, user string, typ domain.PeriodType, count int, revisions domain.Revisions) {
	t.Helper()
	list, err := e.uc.Periods(t.Context(), user, typ)
	if err != nil || len(list.Items) != count {
		t.Fatal("domain periods", list, err)
	}
	wantRevisionMap(t, list.Revisions, revisions)
}

func TestIntegrationDomainSourceRevisionsAreAtomicDurableAndTenantScoped(t *testing.T) {
	e := start(t)
	user, foreign := userID(501), userID(502)
	known := domain.Revisions{"categories": 0, "uploads": 2, "future_source": math.MaxInt64}
	initial := revisionBatch(user, 10, "2026-09", known)
	applyRevisionBatch(t, e, initial)
	applyRevisionBatch(t, e, initial) // Identical equal-version delivery remains allowed.
	old := revisionBatch(user, 9, "2026-09", domain.Revisions{"uploads": 1})
	result, err := e.uc.Upsert(t.Context(), old)
	if err != nil || result.Skipped != 2 {
		t.Fatal(result, err)
	}
	checkRevisionRead(t, e, user, "2026-09", known, known, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 1, known)
	if got, err := e.repo.ReadAnalytics(t.Context(), foreign, domain.PeriodMonth, nil, domain.TransferWith); err != nil || got.State != nil || got.Snapshot != nil {
		t.Fatal("tenant scope", got, err)
	}

	// The domain validator refuses the whole batch before state/header effects.
	tooMany := domain.Revisions{}
	for index := range 33 {
		tooMany[fmt.Sprintf("source_%02d", index)] = 0
	}
	for _, badMap := range []domain.Revisions{{"uploads": -1}, {"Upper": 0}, tooMany} {
		bad := revisionBatch(user, 11, "2026-08", known)
		bad.Snapshots[1].Revisions = badMap
		if _, err := e.uc.Upsert(t.Context(), bad); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal("invalid domain batch accepted", err)
		}
	}
	abort := errors.New("fail before financial transaction commit")
	if err := e.db.Run(t.Context(), func(ctx context.Context) error {
		next := revisionBatch(user, 11, "2026-08", domain.Revisions{"uploads": 3})
		if err := e.repo.UpsertState(ctx, user, next.Version, *next.State); err != nil {
			return err
		}
		if _, err := e.repo.UpsertPeriods(ctx, user, next.Version, next.Snapshots, nil); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	checkRevisionRead(t, e, user, "2026-09", known, known, true)
	checkRevisionRead(t, e, user, "2026-08", known, nil, false)
	checkRevisionList(t, e, user, domain.PeriodMonth, 1, known)
	if _, _, version := rows(t, e, user); version != 10 {
		t.Fatal("failed transaction changed financial row version", version)
	}

	// A separate repository and pool read the durable envelope, not process state.
	reopened, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: e.db.Primary().Config().ConnString(), MaxConns: 2, AppName: "revisions-restart"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	key := "2026-09"
	persisted, err := postgres.NewMetrics(reopened).ReadAnalytics(t.Context(), user, domain.PeriodMonth, &key, domain.TransferWith)
	if err != nil || persisted.State == nil || persisted.Snapshot == nil {
		t.Fatal(persisted, err)
	}
	wantRevisionMap(t, persisted.State.Revisions, known)
	wantRevisionMap(t, persisted.Snapshot.Revisions, known)
	if err := e.db.Run(t.Context(), func(ctx context.Context) error { return e.stones.Purge(ctx, user, e.repo.Purge) }); err != nil {
		t.Fatal(err)
	}
	applyRevisionBatch(t, e, revisionBatch(user, 12, "2026-09", known))
	if headers, _, version := rows(t, e, user); headers != 0 || version != 0 {
		t.Fatal("late provenance recreated deleted facts", headers, version)
	}
}

func TestIntegrationDomainClearingHoldsWholeCalendarProofUntilFinal(t *testing.T) {
	e := start(t)
	user := userID(503)
	known := domain.Revisions{"categories": 0, "uploads": 2, "time_zone": 0}
	applyRevisionBatch(t, e, revisionBatch(user, 1, "2026-09", known))
	clearing := revisionBatch(user, 2, "", nil)
	clearing.State.Recalculating = false // Daily rebuild: source counters did not change.
	applyRevisionBatch(t, e, clearing)
	checkRevisionRead(t, e, user, "2026-09", nil, known, true)
	checkRevisionRead(t, e, user, "2026-08", nil, nil, false)
	checkRevisionList(t, e, user, domain.PeriodMonth, 1, nil)
	part := revisionBatch(user, 3, "2026-08", known)
	part.State.Revisions = nil
	applyRevisionBatch(t, e, part)
	checkRevisionRead(t, e, user, "2026-08", nil, known, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 2, nil)
	applyRevisionBatch(t, e, revisionBatch(user, 4, "", known))
	checkRevisionRead(t, e, user, "2026-09", known, known, true)
	checkRevisionRead(t, e, user, "2026-08", known, known, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 2, known)
	removed := revisionBatch(user, 5, "", nil)
	removed.State.HasData = false
	removed.Removed = []domain.PeriodKey{{Type: domain.PeriodMonth, Key: "2026-09"}, {Type: domain.PeriodMonth, Key: "2026-08"}}
	applyRevisionBatch(t, e, removed)
	checkRevisionRead(t, e, user, "2026-09", nil, nil, false)
	checkRevisionList(t, e, user, domain.PeriodMonth, 0, nil)
	final := revisionBatch(user, 6, "", known)
	final.State.HasData = false
	applyRevisionBatch(t, e, final)
	checkRevisionRead(t, e, user, "2026-09", known, nil, false)
	checkRevisionList(t, e, user, domain.PeriodMonth, 0, known)
}

func TestIntegrationRepositoryRejectsCorruptRevisionEnvelopesWithoutKnownZero(t *testing.T) {
	e := start(t)
	user, foreign := userID(504), userID(505)
	known := domain.Revisions{"categories": 0, "uploads": 0, "time_zone": 0}
	for _, id := range []string{user, foreign} {
		applyRevisionBatch(t, e, revisionBatch(id, 1, "2026-09", known))
	}
	for _, update := range []string{
		`UPDATE user_data_state SET data_revisions=$2 WHERE user_id=$1`,
		`UPDATE period_snapshot SET data_revisions=$2 WHERE user_id=$1`,
	} {
		for _, bad := range []string{
			`{"calc_version":1,"revisions":{"uploads":null}}`,
			`{"calc_version":1,"revisions":{"uploads":0.0}}`,
			`{"calc_version":1,"revisions":{"uploads":true}}`,
			`{"calc_version":1,"revisions":{"uploads":9223372036854775808}}`,
			`{"calc_version":1,"revisions":[]}`,
			`{"calc_version":null,"revisions":{"uploads":0}}`,
		} {
			if _, err := e.db.Primary().Exec(t.Context(), update, user, bad); err != nil {
				t.Fatal(err)
			}
			key := "2026-09"
			if _, err := e.repo.ReadAnalytics(t.Context(), user, domain.PeriodMonth, &key, domain.TransferWith); err == nil || errors.Is(err, domain.ErrInvalid) {
				t.Fatal("storage corruption was accepted or misclassified", bad, err)
			}
			if _, err := e.uc.Periods(t.Context(), user, domain.PeriodMonth); err == nil || errors.Is(err, domain.ErrInvalid) {
				t.Fatal("list storage corruption was accepted or misclassified", bad, err)
			}
			checkRevisionRead(t, e, foreign, key, known, known, true)
		}
		if _, err := e.db.Primary().Exec(t.Context(), update, user, `{"calc_version":1,"revisions":{"categories":0,"uploads":0,"time_zone":0}}`); err != nil {
			t.Fatal(err)
		}
	}
	// SQL NULL is unknown, distinct from the previously known initial zeros.
	if _, err := e.db.Primary().Exec(t.Context(), `UPDATE user_data_state SET data_revisions=NULL WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	checkRevisionRead(t, e, user, "2026-09", nil, known, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 1, nil)
}

func TestIntegrationDomainRevisionReadsStayCoherentDuringConcurrentCommits(t *testing.T) {
	e := start(t)
	user := userID(507)
	applyRevisionBatch(t, e, revisionBatch(user, 1, "2026-09", domain.Revisions{"uploads": 1}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type commitStep struct {
		version     int32
		allowCommit chan struct{}
		observed    chan struct{}
	}
	prepared := make(chan commitStep)
	committed := make(chan int32)
	done := make(chan error, 1)
	go func() {
		for version := int32(2); version <= 5; version++ {
			step := commitStep{version: version, allowCommit: make(chan struct{}), observed: make(chan struct{})}
			err := e.db.Run(ctx, func(txctx context.Context) error {
				u := revisionBatch(user, version, "2026-09", domain.Revisions{"uploads": int64(version)})
				if _, err := e.uc.Upsert(txctx, u); err != nil {
					return err
				}
				// The new facts and envelopes exist in an open transaction.
				// The reader must see the previous committed pair at this point.
				select {
				case prepared <- step:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-step.allowCommit:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err != nil {
				done <- err
				return
			}
			// Run has committed. Do not advance or finish until the reader has
			// observed this intermediate committed version through the repository.
			select {
			case committed <- version:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
			select {
			case <-step.observed:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		done <- nil
	}()
	key := "2026-09"
	observedVersions := map[int32]bool{}
	readVersion := func(before, after int32) {
		t.Helper()
		data, err := e.repo.ReadAnalytics(ctx, user, domain.PeriodMonth, &key, domain.TransferWith)
		if err != nil || data.State == nil || data.Snapshot == nil {
			t.Fatal(data, err)
		}
		actual := data.State.FullMonths
		if (actual != before && actual != after) || data.Snapshot.UnreadLines != actual || data.State.Revisions["uploads"] != int64(actual) || data.Snapshot.Revisions["uploads"] != int64(actual) {
			t.Fatal("mixed or wrong committed financial/provenance version", before, after, data)
		}
		observedVersions[data.State.FullMonths] = true
	}
	for version := int32(2); version <= 5; version++ {
		var step commitStep
		select {
		case step = <-prepared:
		case err := <-done:
			t.Fatal("writer finished before staging every commit", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if step.version != version {
			t.Fatal("unexpected staged version", step.version, version)
		}
		readVersion(version-1, version-1)
		close(step.allowCommit)
		// Race real reads with COMMIT, and always attempt at least one before
		// consuming completion. Each statement may see the old or new version,
		// but never a mixed state/header/source pair across those versions.
		for pending := true; pending; {
			readVersion(version-1, version)
			select {
			case got := <-committed:
				if got != version {
					t.Fatal("unexpected committed version", got, version)
				}
				pending = false
			case err := <-done:
				t.Fatal("writer stopped before commit observation", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			default:
			}
		}
		readVersion(version, version)
		select {
		case err := <-done:
			t.Fatal("writer finished before the reader acknowledged this version", err)
		default:
		}
		close(step.observed)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(observedVersions) != 5 {
		t.Fatal("reader did not observe distinct intermediate commits", observedVersions)
	}
	checkRevisionList(t, e, user, domain.PeriodMonth, 1, domain.Revisions{"uploads": 5})
}

func TestIntegrationDomainPeriodsIntersectEverySelectedHeaderWithState(t *testing.T) {
	e := start(t)
	user := userID(508)
	old := domain.Revisions{"categories": 1, "uploads": 2, "future_source": math.MaxInt64}
	current := domain.Revisions{"categories": 2, "uploads": 2, "future_source": math.MaxInt64}
	applyRevisionBatch(t, e, revisionBatch(user, 1, "2026-09", old))
	applyRevisionBatch(t, e, revisionBatch(user, 2, "2026-08", current))
	checkRevisionRead(t, e, user, "2026-09", current, old, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 2, domain.Revisions{"uploads": 2, "future_source": math.MaxInt64})
	legacy := revisionBatch(user, 3, "2026-10", current)
	for index := range legacy.Snapshots {
		legacy.Snapshots[index].Revisions = nil
	}
	applyRevisionBatch(t, e, legacy)
	// The first sorted header is unknown. Later known headers must never restart
	// the intersection and manufacture a baseline for the whole period list.
	checkRevisionRead(t, e, user, "2026-10", current, nil, true)
	checkRevisionList(t, e, user, domain.PeriodMonth, 3, nil)
	checkRevisionList(t, e, user, domain.PeriodQuarter, 0, current)
}
