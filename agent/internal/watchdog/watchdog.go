// Package watchdog keeps the agent appliance's foreground XLibre/i3 session
// alive. After Ly has started, a healthy appliance has BOTH an agent logind
// graphical session AND an agent-owned i3 process; that is the live display the
// broker's Cua child attaches to. If EITHER disappears for the grace period,
// the watchdog restarts Ly on tty1 so Ly performs its one allowed autologin
// again and rebuilds the session.
//
// The watchdog NEVER launches a display itself: it does not start X, i3, Xvfb,
// or any nested session. Its only recovery lever is restarting ly@tty1.service,
// and it rate-limits even that (three restarts per five minutes) so a session
// that refuses to come up degrades cleanly instead of thrashing tty1 forever.
//
// The "is the session present?" detector and the "restart Ly" action are
// injectable seams, and time is driven through an injectable clock, so the
// grace/rate-limit logic is exercised in tests without real logind or systemd.
package watchdog

import (
	"context"
	"log/slog"
	"time"
)

// Defaults for the recovery policy. Grace matches the brief (30s); the rate
// limit is three Ly restarts per five minutes.
const (
	DefaultGrace        = 30 * time.Second
	DefaultRateLimit    = 3
	DefaultRateWindow   = 5 * time.Minute
	DefaultPollInterval = 3 * time.Second
)

// SessionDetector reports whether the agent's foreground graphical session is
// present. A production implementation requires BOTH an agent logind graphical
// session and an agent-owned i3 process; tests inject a fake.
type SessionDetector interface {
	// Present returns true only when the full agent graphical session is up.
	Present(ctx context.Context) (bool, error)
}

// SessionDetectorFunc adapts a plain function to SessionDetector.
type SessionDetectorFunc func(ctx context.Context) (bool, error)

// Present calls the wrapped function.
func (f SessionDetectorFunc) Present(ctx context.Context) (bool, error) { return f(ctx) }

// Restarter performs the ONE recovery action the watchdog is allowed: restart
// Ly on tty1. It must never start a display any other way.
type Restarter interface {
	// RestartLy restarts ly@tty1.service (and nothing else).
	RestartLy(ctx context.Context) error
}

// RestarterFunc adapts a plain function to Restarter.
type RestarterFunc func(ctx context.Context) error

// RestartLy calls the wrapped function.
func (f RestarterFunc) RestartLy(ctx context.Context) error { return f(ctx) }

// Config wires the watchdog's policy and its injectable seams.
type Config struct {
	// User is the account whose graphical session is guarded (agent). It is
	// carried for logging and by the production detector/restarter wiring.
	User string
	// Grace is how long the session may be absent before a restart. Zero uses
	// DefaultGrace.
	Grace time.Duration
	// RateLimit is the maximum number of restarts allowed within RateWindow.
	// Zero uses DefaultRateLimit.
	RateLimit int
	// RateWindow is the sliding window for the rate limit. Zero uses
	// DefaultRateWindow.
	RateWindow time.Duration
	// PollInterval is how often Run samples the detector. Zero uses
	// DefaultPollInterval.
	PollInterval time.Duration

	// Detector reports whether the agent graphical session is present.
	Detector SessionDetector
	// Restarter restarts ly@tty1.service.
	Restarter Restarter

	// Now returns the current time; nil uses time.Now. Injectable for tests.
	Now func() time.Time

	// Logger receives structured events; nil discards.
	Logger *slog.Logger
}

// Watchdog holds the recovery state machine. It is not safe for concurrent use;
// Run drives it from a single goroutine.
type Watchdog struct {
	user       string
	grace      time.Duration
	rateLimit  int
	rateWindow time.Duration
	poll       time.Duration

	detector  SessionDetector
	restarter Restarter
	now       func() time.Time
	log       *slog.Logger

	// absentSince is the instant the session was first observed missing since
	// the last time it was present (or the last restart). Nil while present.
	absentSince *time.Time
	// restarts holds the timestamps of recent Ly restarts within rateWindow.
	restarts []time.Time
	// degraded latches true once the rate limit blocks a needed restart.
	degraded bool
}

// New builds a Watchdog from cfg, applying defaults for any zero policy value.
func New(cfg Config) *Watchdog {
	w := &Watchdog{
		user:       cfg.User,
		grace:      cfg.Grace,
		rateLimit:  cfg.RateLimit,
		rateWindow: cfg.RateWindow,
		poll:       cfg.PollInterval,
		detector:   cfg.Detector,
		restarter:  cfg.Restarter,
		now:        cfg.Now,
		log:        cfg.Logger,
	}
	if w.grace <= 0 {
		w.grace = DefaultGrace
	}
	if w.rateLimit <= 0 {
		w.rateLimit = DefaultRateLimit
	}
	if w.rateWindow <= 0 {
		w.rateWindow = DefaultRateWindow
	}
	if w.poll <= 0 {
		w.poll = DefaultPollInterval
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.log == nil {
		w.log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return w
}

// action is the decision produced by evaluate.
type action int

const (
	actionNone      action = iota // nothing to do this tick
	actionRestartLy               // grace exceeded and rate limit allows a restart
)

// evaluate advances the state machine for one observation at time now and
// reports the action to take. It is pure with respect to the clock: given the
// same (now, present) sequence it produces the same decisions, which is what
// the grace and rate-limit tests rely on.
//
// Policy:
//   - present            -> clear the absence timer; no action.
//   - absent < grace     -> start/continue the absence timer; no action.
//   - absent >= grace    -> restart Ly, UNLESS the rate limit is hit, in which
//     case latch degraded and take no action (stop thrashing). After a restart
//     the absence timer resets so Ly is given a fresh grace window to rebuild
//     the session before the next restart is considered.
func (w *Watchdog) evaluate(now time.Time, present bool) action {
	if present {
		w.absentSince = nil
		return actionNone
	}
	if w.absentSince == nil {
		t := now
		w.absentSince = &t
		return actionNone
	}
	if now.Sub(*w.absentSince) < w.grace {
		return actionNone
	}

	// Grace exceeded: a restart is warranted. Enforce the sliding-window rate
	// limit first.
	w.pruneRestarts(now)
	if len(w.restarts) >= w.rateLimit {
		w.degraded = true
		return actionNone
	}
	w.restarts = append(w.restarts, now)
	w.absentSince = nil // give Ly a fresh grace window after this restart
	return actionRestartLy
}

// pruneRestarts drops restart timestamps older than the rate window relative to
// now.
func (w *Watchdog) pruneRestarts(now time.Time) {
	cutoff := now.Add(-w.rateWindow)
	kept := w.restarts[:0]
	for _, t := range w.restarts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.restarts = kept
}

// Degraded reports whether the watchdog has stopped restarting Ly because the
// rate limit was reached. Readiness reporting treats this as DEGRADED.
func (w *Watchdog) Degraded() bool { return w.degraded }

// Readiness returns a short human-readable readiness token: "ok" while the
// watchdog is actively guarding, "degraded" once the rate limit has latched.
func (w *Watchdog) Readiness() string {
	if w.degraded {
		return "degraded"
	}
	return "ok"
}

// Run drives the watchdog until ctx is cancelled. Each tick it samples the
// detector and applies evaluate's decision, restarting Ly (only) when the
// policy calls for it. It returns ctx.Err() on cancellation.
func (w *Watchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	// Sample once immediately so a session that is already down is noticed
	// without waiting a full poll interval.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick performs a single observe-decide-act step.
func (w *Watchdog) tick(ctx context.Context) {
	present, err := w.detector.Present(ctx)
	if err != nil {
		// A detection error is treated as "not present": the session cannot be
		// confirmed healthy, so the grace timer runs. This is intentional -- a
		// broken logind/proc read should not mask a dead display forever.
		w.log.Warn("session detection failed", slog.String("user", w.user), slog.Any("err", err))
		present = false
	}
	switch w.evaluate(w.now(), present) {
	case actionRestartLy:
		w.log.Warn("agent session absent past grace; restarting ly@tty1",
			slog.String("user", w.user),
			slog.Int("restarts_in_window", len(w.restarts)))
		if err := w.restarter.RestartLy(ctx); err != nil {
			w.log.Error("ly restart failed", slog.Any("err", err))
		}
	case actionNone:
		if w.degraded {
			w.log.Warn("agent session still absent but restart rate limit reached; degraded",
				slog.String("user", w.user))
		}
	}
}

// discard is an io.Writer sink for the fallback no-op logger.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
