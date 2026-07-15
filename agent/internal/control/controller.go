// Package control implements the local emergency controller for hadron-agent:
// the in-process object that owns a cancellable "generation" every active MCP
// call runs under, the paused flag that rejects new calls, and the fan-out
// that tells both brokers (the unprivileged session broker and the privileged
// root helper) to pause and resume.
//
// A pause is the operator's emergency stop. It must, atomically with respect
// to new calls:
//
//   - reject every subsequent call from BOTH credential classes with
//     api.CodePaused (the gateway consults Enter before routing anything);
//   - cancel every in-flight call by cancelling the current generation's
//     context, which the gateway derives each call's context from;
//   - instruct both brokers to pause via their own lifecycle path.
//
// A resume installs a fresh generation (so calls admitted after resume are not
// tied to the cancelled one) and resumes both brokers.
//
// The controller never executes tools, opens paths, runs as root, or touches
// Docker. It only cancels contexts, flips a flag, and forwards pause/resume to
// injected Broker values. The concrete brokers a deployment injects reach the
// session and root helpers over their NON-authenticated lifecycle path (the
// control socket / session socket), never the auth-gated root call socket;
// modelling them as an interface keeps that wiring decision out of this
// package and lets tests substitute fakes.
package control

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mudler/hadron-desktop/agent/internal/rpc"
)

// Broker is the lifecycle subset of a broker the controller drives on pause
// and resume. Both calls must be idempotent: the controller invokes them
// directly with no de-duplication, and a broker may already be in the target
// state. A deployment injects one Broker per real broker (session and root),
// each reaching its target over a non-authenticated lifecycle path.
type Broker interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

// HealthFunc reports whether a single runtime component is currently healthy.
// It must return quickly and never block indefinitely; the controller calls it
// while assembling a health snapshot and passes the caller's context through so
// a slow component cannot wedge /healthz. It must return only a boolean -- no
// paths, users, tokens, process data, or arguments ever flow through it.
type HealthFunc func(ctx context.Context) bool

// Status is the fully redacted runtime snapshot the controller exposes. It
// carries only a version string and booleans: no paths, users, tokens, process
// data, or tool arguments. It is safe to serialize straight onto /healthz.
type Status struct {
	Version string `json:"version"`
	Gateway bool   `json:"gateway"`
	Session bool   `json:"session"`
	Cua     bool   `json:"cua"`
	Paused  bool   `json:"paused"`
}

// Ready reports whether the runtime is ready to serve: the gateway, session,
// and Cua backends are all healthy AND the runtime is not paused.
func (s Status) Ready() bool {
	return s.Gateway && s.Session && s.Cua && !s.Paused
}

// Config constructs a Controller.
type Config struct {
	// Version is the build version string reported in Status. It is the only
	// non-boolean field a Status ever carries.
	Version string
	// Brokers are told to pause and resume alongside the controller's own
	// generation. Typically two: the session broker and the root helper.
	Brokers []Broker
	// SessionHealthy and CuaHealthy report the health of the session broker
	// and the Cua desktop backend for Status. Either may be nil, in which
	// case that component is reported healthy (a deployment that does not wire
	// a probe should not fail readiness on a missing probe).
	SessionHealthy HealthFunc
	CuaHealthy     HealthFunc
	// Logger receives the redacted status line written on every pause and
	// resume. If nil, a discarding logger is used.
	Logger *slog.Logger
}

// Controller is the emergency stop. It is safe for concurrent use.
type Controller struct {
	version        string
	brokers        []Broker
	sessionHealthy HealthFunc
	cuaHealthy     HealthFunc
	logger         *slog.Logger

	mu     sync.Mutex
	paused bool
	gen    *generation
}

// generation is one epoch of active calls. Every call admitted by Enter while
// this generation is current derives its context from gen.ctx; cancelling gen
// cancels all of them at once.
type generation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newGeneration() *generation {
	ctx, cancel := context.WithCancel(context.Background())
	return &generation{ctx: ctx, cancel: cancel}
}

// New constructs a Controller in the running (not paused) state with a fresh
// generation already installed.
func New(cfg Config) *Controller {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Controller{
		version:        cfg.Version,
		brokers:        cfg.Brokers,
		sessionHealthy: cfg.SessionHealthy,
		cuaHealthy:     cfg.CuaHealthy,
		logger:         logger,
		gen:            newGeneration(),
	}
}

// Enter admits one call under the current generation. On success it returns a
// context derived from reqCtx that is ALSO cancelled if the current generation
// is cancelled (i.e. on pause), a release function the caller must invoke when
// the call finishes, and ok == true. When the controller is paused it returns
// (nil, nil, false) and the caller must reject the call with api.CodePaused.
//
// The paused check and the capture of the current generation happen under the
// same lock that Pause takes to flip paused and swap the generation, so no
// call can slip in between "reject new calls" and "cancel active calls".
func (c *Controller) Enter(reqCtx context.Context) (context.Context, func(), bool) {
	c.mu.Lock()
	if c.paused {
		c.mu.Unlock()
		return nil, nil, false
	}
	gen := c.gen
	c.mu.Unlock()

	ctx, cancel := context.WithCancel(reqCtx)
	// Cancel this call's context if the generation is cancelled (pause),
	// in addition to reqCtx's own cancellation.
	stop := context.AfterFunc(gen.ctx, cancel)
	release := func() {
		stop()
		cancel()
	}
	return ctx, release, true
}

// Paused reports whether the controller is currently paused.
func (c *Controller) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// Pause is the emergency stop. It is idempotent. It flips paused, cancels the
// current generation (cancelling every in-flight call), tells every broker to
// pause, and writes a redacted status line. Broker pause errors are logged but
// do not fail the pause: the local generation is already cancelled and new
// calls are already rejected, which is the safety-critical part.
func (c *Controller) Pause(ctx context.Context) error {
	c.mu.Lock()
	alreadyPaused := c.paused
	c.paused = true
	gen := c.gen
	c.mu.Unlock()

	// Cancel active calls regardless of prior state (idempotent, harmless).
	gen.cancel()

	for i, b := range c.brokers {
		if err := b.Pause(ctx); err != nil {
			c.logger.LogAttrs(ctx, slog.LevelError, "broker pause failed",
				slog.Int("broker_index", i))
		}
	}

	if !alreadyPaused {
		c.logStatus(ctx, "runtime paused")
	}
	return nil
}

// Resume lifts a pause. It is idempotent. It installs a fresh generation so
// calls admitted after resume are not tied to the cancelled one, clears paused,
// tells every broker to resume, and writes a redacted status line.
func (c *Controller) Resume(ctx context.Context) error {
	c.mu.Lock()
	wasPaused := c.paused
	c.paused = false
	c.gen = newGeneration()
	c.mu.Unlock()

	for i, b := range c.brokers {
		if err := b.Resume(ctx); err != nil {
			c.logger.LogAttrs(ctx, slog.LevelError, "broker resume failed",
				slog.Int("broker_index", i))
		}
	}

	if wasPaused {
		c.logStatus(ctx, "runtime resumed")
	}
	return nil
}

// Status assembles a fully redacted runtime snapshot. Gateway is always true
// (the controller is in-process with the gateway, so if this runs the gateway
// is up). Session and Cua come from the injected health probes; a nil probe is
// treated as healthy. Paused reflects the current pause state.
func (c *Controller) Status(ctx context.Context) Status {
	return Status{
		Version: c.version,
		Gateway: true,
		Session: c.probe(ctx, c.sessionHealthy),
		Cua:     c.probe(ctx, c.cuaHealthy),
		Paused:  c.Paused(),
	}
}

func (c *Controller) probe(ctx context.Context, fn HealthFunc) bool {
	if fn == nil {
		return true
	}
	return fn(ctx)
}

// logStatus writes the current redacted status. Only booleans and the version
// string are emitted; no secret ever reaches this line.
func (c *Controller) logStatus(ctx context.Context, msg string) {
	s := c.Status(ctx)
	c.logger.LogAttrs(ctx, slog.LevelWarn, msg,
		slog.String("version", s.Version),
		slog.Bool("gateway", s.Gateway),
		slog.Bool("session", s.Session),
		slog.Bool("cua", s.Cua),
		slog.Bool("paused", s.Paused),
	)
}

// ---------------------------------------------------------------------------
// Control socket handler
// ---------------------------------------------------------------------------

// SocketHandler adapts a Controller to the rpc.Handler interface so a
// Controller can be served on the local control socket (rpc.ControlSocketPath,
// RequireAuth=false). This is the ONLY way the external `control toggle`
// command reaches the controller: it dials the control socket and issues
// /v1/pause or /v1/resume, which land here.
//
// Call is unsupported: the control socket carries lifecycle only, never tool
// execution. Health reports the aggregate readiness and the paused flag.
type SocketHandler struct {
	c *Controller
}

// NewSocketHandler returns an rpc.Handler backed by c for the control socket.
func NewSocketHandler(c *Controller) SocketHandler {
	return SocketHandler{c: c}
}

// Call always fails: the control socket never executes tools.
func (h SocketHandler) Call(_ context.Context, _ rpc.CallRequest, _ string) (*rpc.CallResponse, error) {
	return nil, errControlNoCall
}

// Health reports the controller's aggregate health and paused state.
func (h SocketHandler) Health(ctx context.Context) (rpc.HealthStatus, error) {
	s := h.c.Status(ctx)
	return rpc.HealthStatus{OK: s.Ready(), Paused: s.Paused}, nil
}

// Pause pauses the controller.
func (h SocketHandler) Pause(ctx context.Context) error { return h.c.Pause(ctx) }

// Resume resumes the controller.
func (h SocketHandler) Resume(ctx context.Context) error { return h.c.Resume(ctx) }

// errControlNoCall is returned by SocketHandler.Call; the control socket does
// not execute tools.
var errControlNoCall = controlError("control socket does not execute tool calls")

type controlError string

func (e controlError) Error() string { return string(e) }
