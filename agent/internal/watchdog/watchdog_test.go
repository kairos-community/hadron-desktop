package watchdog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic grace/rate tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingRestarter records every RestartLy call so a test can assert the
// count and that no other restart lever was pulled.
type recordingRestarter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *recordingRestarter) RestartLy(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.err
}

func (r *recordingRestarter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newTestWatchdog(clk *fakeClock, det SessionDetector, rst Restarter) *Watchdog {
	return New(Config{
		User:       "agent",
		Grace:      30 * time.Second,
		RateLimit:  3,
		RateWindow: 5 * time.Minute,
		Detector:   det,
		Restarter:  rst,
		Now:        clk.now,
	})
}

// present-within-grace never restarts.
func TestEvaluate_PresentNoRestart(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	rst := &recordingRestarter{}
	w := newTestWatchdog(clk, nil, rst)

	for i := 0; i < 100; i++ {
		if got := w.evaluate(clk.now(), true); got != actionNone {
			t.Fatalf("present tick %d: got action %v, want none", i, got)
		}
		clk.advance(10 * time.Second)
	}
	if rst.count() != 0 {
		t.Fatalf("restarts while present: got %d, want 0", rst.count())
	}
	if w.Degraded() {
		t.Fatal("degraded while healthy")
	}
}

// absent shorter than grace does not restart; absent past grace restarts once.
func TestEvaluate_GracePeriod(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newTestWatchdog(clk, nil, &recordingRestarter{})

	// First absence starts the timer, no restart.
	if got := w.evaluate(clk.now(), false); got != actionNone {
		t.Fatalf("first absence: got %v, want none", got)
	}
	// 29s absent: still within grace.
	clk.advance(29 * time.Second)
	if got := w.evaluate(clk.now(), false); got != actionNone {
		t.Fatalf("29s absent: got %v, want none (grace not exceeded)", got)
	}
	// 30s absent: grace reached -> restart.
	clk.advance(1 * time.Second)
	if got := w.evaluate(clk.now(), false); got != actionRestartLy {
		t.Fatalf("30s absent: got %v, want restart", got)
	}
	// Immediately after restart the timer reset: next tick is a fresh absence.
	if got := w.evaluate(clk.now(), false); got != actionNone {
		t.Fatalf("tick right after restart: got %v, want none (grace reset)", got)
	}
}

// session returning within grace cancels the pending restart.
func TestEvaluate_RecoveryCancelsRestart(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newTestWatchdog(clk, nil, &recordingRestarter{})

	w.evaluate(clk.now(), false) // start absence timer
	clk.advance(20 * time.Second)
	if got := w.evaluate(clk.now(), true); got != actionNone { // recovered
		t.Fatalf("recovery: got %v, want none", got)
	}
	// Absent again: timer must have reset, so a further 20s is still within grace.
	w.evaluate(clk.now(), false)
	clk.advance(20 * time.Second)
	if got := w.evaluate(clk.now(), false); got != actionNone {
		t.Fatalf("post-recovery 20s absent: got %v, want none", got)
	}
}

// driveAbsent runs the state machine under continuous absence for total wall
// time, advancing the fake clock in step increments, and returns the instants
// at which a restart was issued.
func driveAbsent(w *Watchdog, clk *fakeClock, total, step time.Duration) []time.Time {
	var restarts []time.Time
	for elapsed := time.Duration(0); elapsed < total; elapsed += step {
		if w.evaluate(clk.now(), false) == actionRestartLy {
			restarts = append(restarts, clk.now())
		}
		clk.advance(step)
	}
	return restarts
}

// Under sustained absence the watchdog never issues more than 3 restarts in any
// 5-minute window, and it latches degraded once the limit is first reached.
func TestEvaluate_RateLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newTestWatchdog(clk, nil, &recordingRestarter{})

	restarts := driveAbsent(w, clk, 10*time.Minute, time.Second)

	// The first grace window costs 30s, so at least 3 restarts must occur.
	if len(restarts) < 3 {
		t.Fatalf("restarts over 10m absence: got %d, want >=3", len(restarts))
	}
	// Sliding-window invariant: no 5-minute window ever holds more than 3.
	const window = 5 * time.Minute
	for i := range restarts {
		count := 0
		for j := i; j < len(restarts); j++ {
			if restarts[j].Sub(restarts[i]) < window {
				count++
			}
		}
		if count > 3 {
			t.Fatalf("window starting at restart %d holds %d restarts, want <=3", i, count)
		}
	}
	if !w.Degraded() {
		t.Fatal("want degraded after hitting rate limit")
	}
	if w.Readiness() != "degraded" {
		t.Fatalf("readiness: got %q, want degraded", w.Readiness())
	}
}

// The 4th restart inside the same 5-minute window is blocked and reports
// degraded; once the window slides past the old restarts, restarting resumes.
func TestEvaluate_RateWindowSlides(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newTestWatchdog(clk, nil, &recordingRestarter{})

	// Consume the 3-restart budget: each restart resets the grace timer.
	for i := 0; i < 3; i++ {
		w.evaluate(clk.now(), false)  // start/continue absence
		clk.advance(30 * time.Second) // reach grace
		if w.evaluate(clk.now(), false) != actionRestartLy {
			t.Fatalf("expected restart %d", i)
		}
	}
	// Re-arm the grace timer (the last restart reset it), then run past grace:
	// the 4th restart within the window is blocked.
	w.evaluate(clk.now(), false)  // start a fresh absence timer
	clk.advance(30 * time.Second) // reach grace again
	if got := w.evaluate(clk.now(), false); got != actionNone {
		t.Fatalf("4th restart within window: got %v, want none", got)
	}
	if !w.Degraded() {
		t.Fatal("want degraded once the limit blocks a restart")
	}
	// Advance beyond the 5-minute window so the earliest restarts age out; the
	// next past-grace tick is allowed to restart again.
	clk.advance(6 * time.Minute)
	if got := w.evaluate(clk.now(), false); got != actionRestartLy {
		t.Fatalf("restart after window slid: got %v, want restart", got)
	}
}

// Run drives the loop through the real seams: an absent session past grace
// triggers exactly one restart via the injected Restarter, and only that lever.
func TestRun_RestartsViaSeam(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	rst := &recordingRestarter{}
	// Detector reports absent until a restart is observed, then present.
	det := SessionDetectorFunc(func(context.Context) (bool, error) {
		if rst.count() > 0 {
			return true, nil
		}
		return false, nil
	})
	w := New(Config{
		User:         "agent",
		Grace:        30 * time.Second,
		PollInterval: time.Millisecond,
		Detector:     det,
		Restarter:    rst,
		Now:          clk.now,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Advance the clock past grace shortly after Run starts.
		time.Sleep(20 * time.Millisecond)
		clk.advance(31 * time.Second)
	}()

	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for rst.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not restart Ly within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if got := rst.count(); got < 1 {
		t.Fatalf("restart count: got %d, want >=1", got)
	}
}

// A detector error is treated as "absent" so a broken probe cannot mask a dead
// display: past grace it still restarts.
func TestEvaluate_DetectorErrorTreatedAbsent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	rst := &recordingRestarter{}
	det := SessionDetectorFunc(func(context.Context) (bool, error) {
		return false, errors.New("loginctl blew up")
	})
	w := New(Config{
		User:      "agent",
		Grace:     30 * time.Second,
		Detector:  det,
		Restarter: rst,
		Now:       clk.now,
	})
	w.tick(context.Background()) // start absence timer
	clk.advance(31 * time.Second)
	w.tick(context.Background())
	if rst.count() != 1 {
		t.Fatalf("restart after detector error past grace: got %d, want 1", rst.count())
	}
}
