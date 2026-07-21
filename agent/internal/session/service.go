// Package session implements the UNPRIVILEGED session broker: the component
// that composes the four Phase-2 executors (files, process, the Cua adapter,
// and a control/pause state) into one object that serves the seven public MCP
// tools defined by package api, under the identity of the process it runs in.
//
// It owns four cross-cutting behaviors the individual executors do not:
//
//   - Degradation. computer_use is the only tool backed by the desktop
//     session; when Cua is down it degrades to api.CodeSessionUnavailable
//     while every shell/file tool (terminal, process, read_file, search_files,
//     write_file, patch) stays LIVE. The broker never gates a shell/file call
//     on Cua's health.
//
//   - Pause/resume. A pause atomically (a) rejects new calls with
//     api.CodePaused, (b) cancels every in-flight call by cancelling the
//     current generation context, and (c) terminates every long-running,
//     MCP-owned process group. Resume installs a fresh generation and restarts
//     Cua. Both are idempotent. The atomicity rests on a single mutex: enter()
//     reads (paused, generation) under it, and Pause() flips paused and cancels
//     that same generation under it, so no call can slip between "reject new"
//     and "cancel active".
//
//   - Concurrency. Shell/file calls share a bounded semaphore (eight by
//     default). computer_use is NOT gated by that semaphore: the Cua adapter
//     already funnels every computer_use through its own single seat, and the
//     broker does not double-serialize it.
//
//   - Identity. Every served (successful or truncated) result is stamped with
//     the OS identity the call executed under, drawn from an injected
//     IdentityProvider so the root helper (a later task) can report root while
//     this broker reports the unprivileged user.
//
// Every dependency is injected through Config, over small interfaces the real
// *files.Service, *process.Manager, and a Cua backend satisfy, so the broker is
// testable with hermetic fakes and no real cua-driver, spawning, or root.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/cua"
)

// DefaultOSConcurrency is the number of shell/file calls the broker runs
// concurrently when Config.OSConcurrency is unset.
const DefaultOSConcurrency = 8

// ---------------------------------------------------------------------------
// Injected dependencies
// ---------------------------------------------------------------------------

// FileService is the broker's view of the filesystem executor. *files.Service
// satisfies it.
type FileService interface {
	Read(ctx context.Context, in api.ReadFileInput) api.ReadFileOutput
	Search(ctx context.Context, in api.SearchFilesInput) api.SearchFilesOutput
	Write(ctx context.Context, in api.WriteFileInput) api.WriteFileOutput
	Patch(ctx context.Context, in api.PatchInput) api.PatchOutput
}

// ProcessManager is the broker's view of the shell executor.
// *process.Manager satisfies it.
type ProcessManager interface {
	Bash(ctx context.Context, in api.BashInput) (api.BashOutput, error)
	// Close terminates every tracked process group; used on broker shutdown.
	Close() error
}

// Computer is the broker's view of the Cua computer-use backend, hiding its
// lifecycle (lazy start, reconnect readiness, stop/restart across a pause)
// behind a small surface. *CuaComputer satisfies it in production; tests inject
// a fake.
type Computer interface {
	// ComputerUse executes a computer_use action, starting or reusing the
	// backend as needed. It returns api.CodeSessionUnavailable in the result
	// (never a Go error) when the desktop session is not reachable.
	ComputerUse(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error)
	// Browser executes a browser action against the page in a browser window,
	// starting or reusing the backend the same way ComputerUse does. It shares
	// the backend's single seat, so a snapshot cannot be interleaved with a
	// click on a page that has since moved on.
	Browser(ctx context.Context, in api.BrowserInput) (api.BrowserOutput, error)
	// Ready reports whether a computer_use call could currently reach a live
	// desktop session.
	Ready() bool
	// Stop closes the current backend child (idempotent). The next ComputerUse
	// after Stop starts a fresh one; this is how resume restarts Cua.
	Stop()
	// Close permanently releases the backend (idempotent).
	Close() error
}

// IdentityProvider yields the OS identity the broker's calls execute under. The
// unprivileged broker injects the running user; the root helper injects root.
type IdentityProvider interface {
	Identity() api.Identity
}

// ---------------------------------------------------------------------------
// Broker
// ---------------------------------------------------------------------------

// Config constructs a Broker. Files, Process, and Computer are required;
// Identity and OSConcurrency default when unset.
type Config struct {
	Files    FileService
	Process  ProcessManager
	Computer Computer
	Identity IdentityProvider
	// OSConcurrency bounds concurrent shell/file calls. Defaults to
	// DefaultOSConcurrency.
	OSConcurrency int
}

// gate is enter()'s verdict on whether a call may proceed.
type gate int

const (
	gateLive gate = iota
	gatePaused
	gateClosed
)

// generation is one "epoch" of accepted calls. Every in-flight call derives
// its execution context from the current generation's ctx; Pause cancels it to
// interrupt all of them at once, and Resume replaces it.
type generation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newGeneration(parent context.Context) *generation {
	ctx, cancel := context.WithCancel(parent)
	return &generation{ctx: ctx, cancel: cancel}
}

// Broker is the unprivileged session broker. It is safe for concurrent use.
type Broker struct {
	files    FileService
	process  ProcessManager
	computer Computer
	identity IdentityProvider

	// sem bounds concurrent shell/file calls. computer_use never takes it.
	sem chan struct{}

	// base is the root context every generation derives from; baseCancel is
	// fired once on Close so no generation can outlive the broker.
	base       context.Context
	baseCancel context.CancelFunc

	mu     sync.Mutex // guards everything below
	paused bool
	closed bool
	gen    *generation
}

// New builds a Broker from cfg.
func New(cfg Config) *Broker {
	if cfg.OSConcurrency <= 0 {
		cfg.OSConcurrency = DefaultOSConcurrency
	}
	if cfg.Identity == nil {
		cfg.Identity = OSIdentity{}
	}
	base, baseCancel := context.WithCancel(context.Background())
	return &Broker{
		files:      cfg.Files,
		process:    cfg.Process,
		computer:   cfg.Computer,
		identity:   cfg.Identity,
		sem:        make(chan struct{}, cfg.OSConcurrency),
		base:       base,
		baseCancel: baseCancel,
		gen:        newGeneration(base),
	}
}

// enter returns the current generation context and whether a call may proceed.
// It is the single atomic point where the pause/closed state is observed: the
// returned ctx is the exact context Pause cancels, so a call that passes enter
// just before a pause still has its context cancelled by that pause.
func (b *Broker) enter() (context.Context, gate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.closed:
		return nil, gateClosed
	case b.paused:
		return nil, gatePaused
	default:
		return b.gen.ctx, gateLive
	}
}

// ---------------------------------------------------------------------------
// Pause / Resume / Close
// ---------------------------------------------------------------------------

// Pause atomically rejects new calls, cancels in-flight calls, and terminates
// every long-running MCP-owned process group. It is idempotent. Cua's child is
// left running (its in-flight calls are cancelled via the generation context);
// it is closed and restarted only on Resume.
func (b *Broker) Pause(_ context.Context) error {
	b.mu.Lock()
	if b.closed || b.paused {
		b.mu.Unlock()
		return nil
	}
	b.paused = true
	b.gen.cancel() // cancels every in-flight call's execution context
	b.mu.Unlock()

	// Nothing else to terminate: without the process tool every call is a
	// one-shot bash invocation whose process group the generation cancellation
	// above already reaps. There are no detached, tracked processes left.
	return nil
}

// Resume installs a fresh generation, resumes accepting calls, and restarts
// Cua (by stopping the old child so the next computer_use starts a fresh one).
// It is idempotent.
func (b *Broker) Resume(_ context.Context) error {
	b.mu.Lock()
	if b.closed || !b.paused {
		b.mu.Unlock()
		return nil
	}
	b.paused = false
	b.gen = newGeneration(b.base)
	b.mu.Unlock()

	b.computer.Stop()
	return nil
}

// Close permanently shuts the broker down: it rejects all further calls,
// cancels in-flight ones, and releases the process and Cua backends. It is
// idempotent.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.gen.cancel()
	b.baseCancel()
	b.mu.Unlock()

	perr := b.process.Close()
	cerr := b.computer.Close()
	if perr != nil {
		return perr
	}
	return cerr
}

// Status is a snapshot of the broker's control state, for a health endpoint.
type Status struct {
	Paused   bool `json:"paused"`
	CuaReady bool `json:"cua_ready"`
}

// Status reports the broker's current pause state and Cua readiness.
func (b *Broker) Status() Status {
	b.mu.Lock()
	paused := b.paused
	b.mu.Unlock()
	return Status{Paused: paused, CuaReady: b.computer.Ready()}
}

// ---------------------------------------------------------------------------
// Guarded dispatch
// ---------------------------------------------------------------------------

// guard runs one tool call under the broker's pause/concurrency/identity
// discipline. useSem selects whether the call takes the shared shell/file
// semaphore (true for the six OS tools, false for computer_use, which the Cua
// adapter already single-seats). metaOf yields a pointer to the embedded
// ResultMeta of the tool's output type so guard can inject a rejection meta or
// stamp identity without knowing the concrete type.
func guard[T any](
	b *Broker,
	ctx context.Context,
	useSem bool,
	metaOf func(*T) *api.ResultMeta,
	run func(ctx context.Context) T,
) T {
	var out T

	genCtx, g := b.enter()
	switch g {
	case gatePaused:
		*metaOf(&out) = pausedMeta()
		return out
	case gateClosed:
		*metaOf(&out) = closedMeta()
		return out
	}

	if useSem {
		select {
		case b.sem <- struct{}{}:
		case <-ctx.Done():
			*metaOf(&out) = queuedCancelMeta(ctx)
			return out
		case <-genCtx.Done():
			// Paused (or closed) while queued for a slot.
			*metaOf(&out) = pausedMeta()
			return out
		}
		defer func() { <-b.sem }()
		// The select can pick the semaphore even when genCtx is already done;
		// re-check so a call cannot start executing after a pause.
		if genCtx.Err() != nil {
			*metaOf(&out) = pausedMeta()
			return out
		}
	}

	mctx, cancel := mergeCancel(ctx, genCtx)
	defer cancel()

	out = run(mctx)
	b.finish(metaOf(&out), ctx, genCtx)
	return out
}

// finish post-processes a completed call's ResultMeta: it converts a
// pause-cancellation into a clean api.CodePaused, and stamps the execution
// identity onto served (success/truncated) results.
func (b *Broker) finish(meta *api.ResultMeta, ctx, genCtx context.Context) {
	// If the generation was cancelled by a pause (genCtx done) while the
	// caller's own context is still live, the underlying executor was
	// interrupted by that pause; report it as PAUSED rather than a bare
	// deadline/internal error.
	if genCtx.Err() != nil && ctx.Err() == nil && isInterruptCode(meta.Code) {
		*meta = pausedMeta()
		return
	}
	if meta.Identity == nil && isServed(meta.Code) {
		id := b.identity.Identity()
		meta.Identity = &id
	}
}

// mergeCancel returns a context cancelled when either parent or other is
// cancelled, plus a cancel func that stops the linkage and releases resources.
func mergeCancel(parent, other context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(other, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func isServed(c api.ErrorCode) bool {
	return c == "" || c == api.CodeOutputTruncated
}

// isInterruptCode reports whether c is one an executor emits when its context
// is cancelled mid-call (files map a cancel to INTERNAL and a deadline to
// DEADLINE_EXCEEDED; process/terminal map either to DEADLINE_EXCEEDED).
func isInterruptCode(c api.ErrorCode) bool {
	return c == api.CodeDeadlineExceeded || c == api.CodeInternal
}

func pausedMeta() api.ResultMeta {
	return api.ResultMeta{Code: api.CodePaused, Message: "session is paused", Retryable: true}
}

func closedMeta() api.ResultMeta {
	return api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "session broker is shut down", Retryable: false}
}

func queuedCancelMeta(ctx context.Context) api.ResultMeta {
	return api.ResultMeta{
		Code:      api.CodeDeadlineExceeded,
		Message:   "cancelled while queued for an execution slot: " + ctx.Err().Error(),
		Retryable: true,
	}
}

// ---------------------------------------------------------------------------
// Typed per-tool methods
// ---------------------------------------------------------------------------

// ComputerUse serves the computer_use tool. It is NOT gated by the shell/file
// semaphore: the Cua adapter behind Computer already funnels every call through
// a single seat, so the broker does not double-serialize it.
func (b *Broker) ComputerUse(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	return guard(b, ctx, false,
		func(o *api.ComputerUseOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.ComputerUseOutput {
			out, _ := b.computer.ComputerUse(mctx, in)
			return out
		}), nil
}

// Browser serves the browser tool. Like ComputerUse it is NOT gated by the
// shell/file semaphore: the Cua adapter behind Computer funnels both through
// the same single seat, so the broker does not double-serialize them.
func (b *Broker) Browser(ctx context.Context, in api.BrowserInput) (api.BrowserOutput, error) {
	return guard(b, ctx, false,
		func(o *api.BrowserOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.BrowserOutput {
			out, _ := b.computer.Browser(mctx, in)
			return out
		}), nil
}

// Bash serves the bash tool.
func (b *Broker) Bash(ctx context.Context, in api.BashInput) (api.BashOutput, error) {
	return guard(b, ctx, true,
		func(o *api.BashOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.BashOutput {
			out, _ := b.process.Bash(mctx, in)
			return out
		}), nil
}

func (b *Broker) ReadFile(ctx context.Context, in api.ReadFileInput) (api.ReadFileOutput, error) {
	return guard(b, ctx, true,
		func(o *api.ReadFileOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.ReadFileOutput {
			return b.files.Read(mctx, in)
		}), nil
}

// SearchFiles serves the search_files tool.
func (b *Broker) SearchFiles(ctx context.Context, in api.SearchFilesInput) (api.SearchFilesOutput, error) {
	return guard(b, ctx, true,
		func(o *api.SearchFilesOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.SearchFilesOutput {
			return b.files.Search(mctx, in)
		}), nil
}

// WriteFile serves the write_file tool.
func (b *Broker) WriteFile(ctx context.Context, in api.WriteFileInput) (api.WriteFileOutput, error) {
	return guard(b, ctx, true,
		func(o *api.WriteFileOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.WriteFileOutput {
			return b.files.Write(mctx, in)
		}), nil
}

// Patch serves the patch tool.
func (b *Broker) Patch(ctx context.Context, in api.PatchInput) (api.PatchOutput, error) {
	return guard(b, ctx, true,
		func(o *api.PatchOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.PatchOutput {
			return b.files.Patch(mctx, in)
		}), nil
}

// ---------------------------------------------------------------------------
// Call dispatch (fits rpc.Handler.Call: tool name + raw JSON args -> result)
// ---------------------------------------------------------------------------

// Call dispatches one tool by name, decoding args into the tool's typed input
// and returning the serialized MCP result. It never returns a Go error for a
// tool-level failure: an unknown tool or undecodable args come back as an
// api.CodeInvalidArgument result, matching the in-band error convention every
// tool uses. A non-nil error is reserved for an internal marshalling failure.
func (b *Broker) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	switch tool {
	case api.ToolComputerUse:
		return dispatchCall(args, func(in api.ComputerUseInput) any {
			out, _ := b.ComputerUse(ctx, in)
			return out
		})
	case api.ToolBash:
		return dispatchCall(args, func(in api.BashInput) any {
			out, _ := b.Bash(ctx, in)
			return out
		})
	case api.ToolReadFile:
		return dispatchCall(args, func(in api.ReadFileInput) any {
			out, _ := b.ReadFile(ctx, in)
			return out
		})
	case api.ToolSearchFiles:
		return dispatchCall(args, func(in api.SearchFilesInput) any {
			out, _ := b.SearchFiles(ctx, in)
			return out
		})
	case api.ToolWriteFile:
		return dispatchCall(args, func(in api.WriteFileInput) any {
			out, _ := b.WriteFile(ctx, in)
			return out
		})
	case api.ToolPatch:
		return dispatchCall(args, func(in api.PatchInput) any {
			out, _ := b.Patch(ctx, in)
			return out
		})
	case api.ToolBrowser:
		return dispatchCall(args, func(in api.BrowserInput) any {
			out, _ := b.Browser(ctx, in)
			return out
		})
	default:
		return toResult(api.ResultMeta{
			Code:    api.CodeInvalidArgument,
			Message: fmt.Sprintf("unknown tool %q", tool),
		})
	}
}

// dispatchCall decodes args into In, runs the tool, and serializes the result.
func dispatchCall[In any](args json.RawMessage, run func(In) any) (*mcp.CallToolResult, error) {
	var in In
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toResult(api.ResultMeta{
				Code:    api.CodeInvalidArgument,
				Message: "invalid arguments: " + err.Error(),
			})
		}
	}
	return toResult(run(in))
}

// toResult serializes a tool output into an mcp.CallToolResult the same way the
// MCP SDK does for these tools: the JSON goes in StructuredContent and, since
// the tools carry their error in-band via ResultMeta.Code, IsError stays false.
func toResult(out any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("session: marshal tool result: %w", err)
	}
	raw := json.RawMessage(data)
	return &mcp.CallToolResult{
		StructuredContent: raw,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil
}

// ---------------------------------------------------------------------------
// Production Cua backend
// ---------------------------------------------------------------------------

// requiredCuaEnv is the set of session variables that must be present in the
// broker's environment before Cua may be started. Their absence means no
// desktop session is reachable, so computer_use degrades without spawning
// anything.
var requiredCuaEnv = []string{"DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS"}

// cuaChildArgs and cuaChildEnv are the fixed invocation of cua-driver: the MCP
// server mode with the daemon-relaunch guard off, telemetry and update checks
// disabled, and accessibility advertised in full.
var (
	cuaChildArgs = []string{"mcp", "--no-daemon-relaunch"}
	cuaChildEnv  = []string{
		"CUA_DRIVER_RS_TELEMETRY_ENABLED=false",
		"CUA_DRIVER_RS_UPDATE_CHECK=false",
		"CUA_DRIVER_RS_A11Y_ADVERTISE_MODE=all",
	}
)

// CuaConfig configures a CuaComputer.
type CuaConfig struct {
	// Binary is the cua-driver executable; defaults to "cua-driver".
	Binary string
	// Getenv reads the required session variables; defaults to os.Getenv.
	Getenv func(string) string
	// StartTimeout bounds the child connect handshake; defaults to 30s.
	StartTimeout time.Duration
}

// CuaComputer is the production Computer: it starts a cua-driver child on
// demand (only when the desktop session variables are present), wraps it in a
// cua.Adapter, tracks readiness via the client's reconnect callback, and can
// stop/restart the child across a pause. It is safe for concurrent use.
type CuaComputer struct {
	binary       string
	getenv       func(string) string
	startTimeout time.Duration

	startMu sync.Mutex // serializes lazy starts

	mu      sync.Mutex
	client  *cua.Client
	adapter *cua.Adapter
	ready   bool
	closed  bool
}

// NewCuaComputer builds a CuaComputer from cfg.
func NewCuaComputer(cfg CuaConfig) *CuaComputer {
	if cfg.Binary == "" {
		cfg.Binary = "cua-driver"
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 30 * time.Second
	}
	return &CuaComputer{
		binary:       cfg.Binary,
		getenv:       cfg.Getenv,
		startTimeout: cfg.StartTimeout,
	}
}

// ComputerUse ensures a live backend and forwards the call to the adapter.
func (c *CuaComputer) ComputerUse(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	adapter, meta := c.ensure(ctx)
	if meta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: meta}, nil
	}
	// The adapter itself returns SESSION_UNAVAILABLE if the child died after we
	// handed back a reference (e.g. a reconnect is still in flight).
	return adapter.ComputerUse(ctx, in)
}

// Browser ensures a live backend and forwards the call to the adapter.
func (c *CuaComputer) Browser(ctx context.Context, in api.BrowserInput) (api.BrowserOutput, error) {
	adapter, meta := c.ensure(ctx)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	return adapter.Browser(ctx, in)
}

// ensure returns a ready adapter, starting the child on first use. It returns a
// non-empty ResultMeta (never a Go error) when the backend cannot be provided.
func (c *CuaComputer) ensure(ctx context.Context) (*cua.Adapter, api.ResultMeta) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "computer-use backend is shut down"}
	}
	if c.adapter != nil {
		a := c.adapter
		c.mu.Unlock()
		return a, api.ResultMeta{}
	}
	c.mu.Unlock()

	// Serialize concurrent first-use starts; re-check state once we hold it.
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "computer-use backend is shut down"}
	}
	if c.adapter != nil {
		a := c.adapter
		c.mu.Unlock()
		return a, api.ResultMeta{}
	}
	c.mu.Unlock()

	for _, key := range requiredCuaEnv {
		if c.getenv(key) == "" {
			return nil, api.ResultMeta{
				Code:      api.CodeSessionUnavailable,
				Message:   fmt.Sprintf("desktop session unavailable: %s is not set", key),
				Retryable: true,
			}
		}
	}

	startCtx, cancel := context.WithTimeout(ctx, c.startTimeout)
	defer cancel()
	client, err := cua.Start(startCtx, cua.Options{
		Binary:  c.binary,
		Args:    cuaChildArgs,
		Env:     cuaChildEnv,
		OnReady: c.onReady,
	})
	if err != nil {
		return nil, api.ResultMeta{
			Code:      api.CodeSessionUnavailable,
			Message:   "failed to start the computer-use backend: " + err.Error(),
			Retryable: true,
		}
	}
	adapter := cua.NewAdapter(client)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = client.Close()
		return nil, api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "computer-use backend is shut down"}
	}
	c.client = client
	c.adapter = adapter
	c.ready = true
	c.mu.Unlock()
	return adapter, api.ResultMeta{}
}

// onReady is the cua.Client reconnect callback: it keeps ready in sync with the
// child's connected state.
func (c *CuaComputer) onReady(connected bool) {
	c.mu.Lock()
	if !c.closed {
		c.ready = connected
	}
	c.mu.Unlock()
}

// Ready reports whether the backend currently believes it can serve calls.
func (c *CuaComputer) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.closed
}

// Stop closes the current child so the next ComputerUse starts a fresh one.
func (c *CuaComputer) Stop() {
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.adapter = nil
	c.ready = false
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// Close permanently releases the backend.
func (c *CuaComputer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	client := c.client
	c.client = nil
	c.adapter = nil
	c.ready = false
	c.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Production identity
// ---------------------------------------------------------------------------

// OSIdentity reports the identity of the running process: its numeric uid and,
// when resolvable, the matching username. It is the default IdentityProvider
// for the unprivileged broker.
type OSIdentity struct{}

// Identity returns the running process's OS identity.
func (OSIdentity) Identity() api.Identity {
	id := api.Identity{UID: os.Getuid()}
	if u, err := user.Current(); err == nil {
		id.User = u.Username
	}
	return id
}
