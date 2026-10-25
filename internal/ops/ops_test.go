package ops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnitReadinessFollowsCheck(t *testing.T) {
	var fail atomic.Bool
	changes := make(chan bool, 10)
	r := &Readiness{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error {
		if fail.Load() {
			return errors.New("db down")
		}
		return nil
	}, func(ok bool) { changes <- ok })

	if !<-changes {
		t.Fatal("first result must be reported: ready")
	}
	fail.Store(true)
	if <-changes {
		t.Fatal("want not ready after the check fails")
	}

	h := Handler(NewRegistry(), r)
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/metrics": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}

// After Drain neither a passing check nor a stale result makes the process ready again:
// /readyz answers 503 for the whole SHUTDOWN_DELAY while liveness stays up.
func TestUnitDrainKeepsReadinessDownWhileTheCheckPasses(t *testing.T) {
	r := &Readiness{}
	changes := make(chan bool, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error { return nil }, func(ok bool) { changes <- ok })
	}()
	if !<-changes || !r.Ready() {
		t.Fatal("passing check did not report ready")
	}
	h := Handler(NewRegistry(), r)
	status := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if status("/readyz") != http.StatusOK {
		t.Fatal("ready process answered not ready")
	}
	r.Drain()
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("draining process still ready")
	}
	// Several more passing checks must not bring readiness back.
	deadline := time.After(50 * time.Millisecond)
	for wait := true; wait; {
		select {
		case ok := <-changes:
			if ok {
				t.Fatal("watch reported ready after drain")
			}
		case <-deadline:
			wait = false
		}
	}
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable || status("/healthz") != http.StatusOK {
		t.Fatal("drain did not hold or changed liveness")
	}
	cancel()
	<-done
}

// An onChange(true) already in flight when Drain is called finishes before Drain
// returns; nothing reports ready afterwards (the race found in review of FIN-275).
func TestUnitDrainWaitsForInFlightReadyNotification(t *testing.T) {
	r := &Readiness{}
	entered, release := make(chan struct{}), make(chan struct{})
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		first := true
		r.Watch(ctx, time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error { return nil }, func(ok bool) {
			if first {
				first = false
				close(entered)
				<-release
			}
			if ok {
				record("ready")
			}
		})
	}()
	<-entered
	drained := make(chan struct{})
	go func() { r.Drain(); record("drained"); close(drained) }()
	select {
	case <-drained: // a correct Drain cannot return here: Watch holds the lock
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-drained
	cancel()
	<-watched
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "ready" || events[1] != "drained" {
		t.Fatalf("events %v, want [ready drained]: Drain returned before the in-flight notification", events)
	}
	if r.Ready() {
		t.Fatal("ready after drain")
	}
}

// blockingHandler holds the first "ready" log record until release is closed.
type blockingHandler struct {
	slog.Handler
	entered, release chan struct{}
	once             *sync.Once
}

func (blockingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h blockingHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "ready" {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return nil
}

// Review repro of FIN-275: Watch logs "ready" and is held there, Drain is called, then
// the log is released. The ready notification must not be delivered after Drain returned.
func TestUnitDrainDuringReadyLogDeliversNoLateReady(t *testing.T) {
	r := &Readiness{}
	h := blockingHandler{Handler: slog.DiscardHandler, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		r.Watch(ctx, time.Millisecond, slog.New(h), func(context.Context) error { return nil }, func(ok bool) {
			if ok {
				record("ready")
			}
		})
	}()
	<-h.entered
	drained := make(chan struct{})
	go func() { r.Drain(); record("drained"); close(drained) }()
	select {
	case <-drained:
	case <-time.After(200 * time.Millisecond):
	}
	close(h.release)
	<-drained
	cancel()
	<-watched
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "ready" || events[1] != "drained" || r.Ready() {
		t.Fatalf("events %v ready=%v: a ready notification arrived after Drain returned", events, r.Ready())
	}
}
