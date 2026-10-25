package ops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
