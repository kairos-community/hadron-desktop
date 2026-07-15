// Package roothelper implements the PRIVILEGED root helper: the independently
// authenticated broker that serves the six OS tools (terminal, process,
// read_file, search_files, write_file, patch) under the root identity.
//
// It is the appliance's privilege boundary and is deliberately paranoid. In
// production it sits behind the ROOT Unix socket; the public gateway proxies an
// admin caller's OS tool calls to it, forwarding the caller's raw Authorization
// header. This helper NEVER trusts the gateway's say-so: it RE-VERIFIES that raw
// bearer LOCALLY against its OWN admin digest set (defense in depth) before any
// call reaches an executor. There is no scope, boolean, or other field a caller
// can present as a substitute for the raw admin bearer -- the only accepted
// authority is a bearer that this helper's own auth.Verifier accepts as class
// admin.
//
// It differs from the unprivileged session broker (package session) in three
// ways, and is a separate package for exactly those reasons:
//
//   - It serves ONLY the six OS tools. computer_use is refused with FORBIDDEN
//     (an admin's computer_use goes to the unprivileged session broker, never
//     here), and so is any tool name outside the six. That rejection happens
//     BEFORE authentication, so a non-OS tool never even triggers a bearer
//     check, let alone an executor.
//
//   - It authenticates every dispatch itself. The order is: reject a non-OS
//     tool (cheap, no secret), then verify the admin bearer (constant-time via
//     auth.Verifier), then dispatch. No dispatch path skips the verify step.
//
//   - Its execution identity is the fixed root identity: every served result is
//     stamped uid=0, user=root, and (because only an admin bearer can reach a
//     dispatch) credential=admin.
//
// It shares the session broker's pause discipline -- a pause atomically rejects
// new calls, cancels in-flight calls via a generation context, and terminates
// tracked root process groups -- but reimplements it here rather than importing
// the session broker's internals.
package roothelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// DefaultOSConcurrency is the number of OS calls the helper runs concurrently
// when Config.OSConcurrency is unset.
const DefaultOSConcurrency = 8

// ---------------------------------------------------------------------------
// Injected dependencies
// ---------------------------------------------------------------------------

// FileService is the helper's view of the (root-owned) filesystem executor.
// *files.Service satisfies it.
type FileService interface {
	Read(ctx context.Context, in api.ReadFileInput) api.ReadFileOutput
	Search(ctx context.Context, in api.SearchFilesInput) api.SearchFilesOutput
	Write(ctx context.Context, in api.WriteFileInput) api.WriteFileOutput
	Patch(ctx context.Context, in api.PatchInput) api.PatchOutput
}

// ProcessManager is the helper's view of the (root-owned) process/terminal
// executor. *process.Manager satisfies it.
type ProcessManager interface {
	Terminal(ctx context.Context, in api.TerminalInput) (api.TerminalOutput, error)
	Process(ctx context.Context, in api.ProcessInput) (api.ProcessOutput, error)
	// Close terminates every tracked process group; used on helper shutdown.
	Close() error
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// rootIdentity is the fixed OS identity every successful root-helper result
// carries. Because only an admin bearer can reach a dispatch, the accompanying
// credential class is always admin (see identityDescriptor).
var rootIdentity = api.Identity{UID: 0, User: "root"}

// identityDescriptor renders id plus the credential class that authorized the
// call in the canonical "uid=%d,user=%s,credential=%s" form the root helper
// guarantees. Only an admin bearer can reach a served result here, so the
// credential is fixed to admin.
func identityDescriptor(id api.Identity) string {
	return fmt.Sprintf("uid=%d,user=%s,credential=%s", id.UID, id.User, auth.ClassAdmin)
}

// ---------------------------------------------------------------------------
// OS tool set
// ---------------------------------------------------------------------------

// osTools is the closed set of the six OS tools this helper serves. Every other
// tool name -- including computer_use -- is refused before any authentication.
var osTools = map[string]struct{}{
	api.ToolTerminal:    {},
	api.ToolProcess:     {},
	api.ToolReadFile:    {},
	api.ToolSearchFiles: {},
	api.ToolWriteFile:   {},
	api.ToolPatch:       {},
}

func isOSTool(tool string) bool {
	_, ok := osTools[tool]
	return ok
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

// Config constructs a Helper. Verifier, Files, and Process are all required;
// OSConcurrency defaults when unset.
type Config struct {
	// Verifier verifies the raw admin bearer LOCALLY. It must be configured
	// with this helper's own admin digest set; a nil Verifier is rejected so
	// the helper can never accidentally run without authentication.
	Verifier *auth.Verifier
	Files    FileService
	Process  ProcessManager
	// OSConcurrency bounds concurrent OS calls. Defaults to
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

// generation is one "epoch" of accepted calls. Every in-flight call derives its
// execution context from the current generation's ctx; Pause cancels it to
// interrupt all of them at once, and Resume replaces it.
type generation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newGeneration(parent context.Context) *generation {
	ctx, cancel := context.WithCancel(parent)
	return &generation{ctx: ctx, cancel: cancel}
}

// Helper is the privileged root helper. It is safe for concurrent use.
type Helper struct {
	verifier *auth.Verifier
	files    FileService
	process  ProcessManager

	// sem bounds concurrent OS calls.
	sem chan struct{}

	// base is the root context every generation derives from; baseCancel is
	// fired once on Close so no generation can outlive the helper.
	base       context.Context
	baseCancel context.CancelFunc

	mu      sync.Mutex // guards everything below
	paused  bool
	closed  bool
	gen     *generation
	procIDs map[string]struct{} // long-running root processes started via this helper
}

// New builds a Helper from cfg. It requires a Verifier, a FileService, and a
// ProcessManager: a helper without any of these could not safely authenticate
// or execute, so New refuses to build one.
func New(cfg Config) (*Helper, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("roothelper: a Verifier is required")
	}
	if cfg.Files == nil {
		return nil, errors.New("roothelper: a FileService is required")
	}
	if cfg.Process == nil {
		return nil, errors.New("roothelper: a ProcessManager is required")
	}
	if cfg.OSConcurrency <= 0 {
		cfg.OSConcurrency = DefaultOSConcurrency
	}
	base, baseCancel := context.WithCancel(context.Background())
	return &Helper{
		verifier:   cfg.Verifier,
		files:      cfg.Files,
		process:    cfg.Process,
		sem:        make(chan struct{}, cfg.OSConcurrency),
		base:       base,
		baseCancel: baseCancel,
		gen:        newGeneration(base),
		procIDs:    make(map[string]struct{}),
	}, nil
}

// enter returns the current generation context and whether a call may proceed.
// It is the single atomic point where the pause/closed state is observed: the
// returned ctx is the exact context Pause cancels, so a call that passes enter
// just before a pause still has its context cancelled by that pause.
func (h *Helper) enter() (context.Context, gate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return nil, gateClosed
	case h.paused:
		return nil, gatePaused
	default:
		return h.gen.ctx, gateLive
	}
}

// ---------------------------------------------------------------------------
// Authentication (re-verified locally for every dispatch)
// ---------------------------------------------------------------------------

// errMissingBearer means no Authorization header was presented at all. It maps,
// like every unrecognized auth error, to UNAUTHENTICATED via auth.Code.
var errMissingBearer = errors.New("roothelper: authentication required")

// verifyAdmin extracts the raw bearer from authHeader and verifies it LOCALLY
// as class admin using the helper's own constant-time Verifier. It returns nil
// only when the bearer is present, well-formed, of the admin class, and matches
// a configured admin digest. Every other case returns an error whose api code
// (via auth.Code) the caller reports without ever reaching an executor.
func (h *Helper) verifyAdmin(authHeader string) error {
	if strings.TrimSpace(authHeader) == "" {
		return errMissingBearer
	}
	raw, ok := rawBearer(authHeader)
	if !ok {
		// A header that is not a "Bearer <token>" is treated as a malformed
		// bearer (UNAUTHENTICATED); it is not, and cannot be substituted for,
		// a valid admin credential.
		return auth.ErrMalformedBearer
	}
	_, err := h.verifier.Verify(raw, auth.ClassAdmin)
	return err
}

// rawBearer extracts the token from an "Authorization: Bearer <token>" header
// value, returning ok=false if the scheme is not Bearer or the token is empty.
// The scheme match is case-insensitive per RFC 7235; the token itself is passed
// verbatim to the constant-time Verifier.
func rawBearer(authHeader string) (string, bool) {
	const scheme = "bearer "
	h := strings.TrimSpace(authHeader)
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(scheme):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// authErrorMeta builds the ResultMeta for a failed local admin verification. It
// surfaces only the stable api code and a fixed, safe message -- never the raw
// bearer or an internal detail.
func authErrorMeta(err error) api.ResultMeta {
	code := auth.Code(err)
	msg := "admin authentication required"
	if code == api.CodeForbidden {
		msg = "admin credential required for the root helper"
	}
	return api.ResultMeta{Code: code, Message: msg, Retryable: false}
}

// ---------------------------------------------------------------------------
// Pause / Resume / Close
// ---------------------------------------------------------------------------

// Pause atomically rejects new calls, cancels in-flight calls, and terminates
// every long-running MCP-owned root process group. It is idempotent.
func (h *Helper) Pause(_ context.Context) error {
	h.mu.Lock()
	if h.closed || h.paused {
		h.mu.Unlock()
		return nil
	}
	h.paused = true
	h.gen.cancel() // cancels every in-flight call's execution context
	ids := make([]string, 0, len(h.procIDs))
	for id := range h.procIDs {
		ids = append(ids, id)
	}
	h.mu.Unlock()

	// Terminate long-running process groups. One-shot terminal calls are torn
	// down by the generation cancellation above; only detached, tracked
	// processes need an explicit terminate.
	for _, id := range ids {
		_, _ = h.process.Process(context.Background(), api.ProcessInput{
			Action:    api.ProcessTerminate,
			ProcessID: id,
		})
	}
	return nil
}

// Resume installs a fresh generation and resumes accepting calls. It is
// idempotent.
func (h *Helper) Resume(_ context.Context) error {
	h.mu.Lock()
	if h.closed || !h.paused {
		h.mu.Unlock()
		return nil
	}
	h.paused = false
	h.gen = newGeneration(h.base)
	h.mu.Unlock()
	return nil
}

// Close permanently shuts the helper down: it rejects all further calls,
// cancels in-flight ones, and releases the process backend. It is idempotent.
func (h *Helper) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.gen.cancel()
	h.baseCancel()
	h.mu.Unlock()

	return h.process.Close()
}

// Status is a snapshot of the helper's control state, for a health endpoint.
type Status struct {
	Paused bool `json:"paused"`
}

// Status reports the helper's current pause state.
func (h *Helper) Status() Status {
	h.mu.Lock()
	paused := h.paused
	h.mu.Unlock()
	return Status{Paused: paused}
}

// ---------------------------------------------------------------------------
// Guarded dispatch
// ---------------------------------------------------------------------------

// guard runs one OS tool call under the helper's pause/concurrency/identity
// discipline. Every OS call takes the shared semaphore. metaOf yields a pointer
// to the embedded ResultMeta of the tool's output type so guard can inject a
// rejection meta or stamp identity without knowing the concrete type.
//
// guard is reached ONLY after verifyAdmin has succeeded for this call; it
// performs no authentication itself and must never be called on an
// unauthenticated path.
func guard[T any](
	h *Helper,
	ctx context.Context,
	metaOf func(*T) *api.ResultMeta,
	run func(ctx context.Context) T,
) T {
	var out T

	genCtx, g := h.enter()
	switch g {
	case gatePaused:
		*metaOf(&out) = pausedMeta()
		return out
	case gateClosed:
		*metaOf(&out) = closedMeta()
		return out
	}

	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		*metaOf(&out) = queuedCancelMeta(ctx)
		return out
	case <-genCtx.Done():
		// Paused (or closed) while queued for a slot.
		*metaOf(&out) = pausedMeta()
		return out
	}
	defer func() { <-h.sem }()
	// The select can pick the semaphore even when genCtx is already done;
	// re-check so a call cannot start executing after a pause.
	if genCtx.Err() != nil {
		*metaOf(&out) = pausedMeta()
		return out
	}

	mctx, cancel := mergeCancel(ctx, genCtx)
	defer cancel()

	out = run(mctx)
	h.finish(metaOf(&out), ctx, genCtx)
	return out
}

// finish post-processes a completed call's ResultMeta: it converts a
// pause-cancellation into a clean api.CodePaused, and stamps the fixed root
// execution identity onto served (success/truncated) results.
func (h *Helper) finish(meta *api.ResultMeta, ctx, genCtx context.Context) {
	if genCtx.Err() != nil && ctx.Err() == nil && isInterruptCode(meta.Code) {
		*meta = pausedMeta()
		return
	}
	if meta.Identity == nil && isServed(meta.Code) {
		id := rootIdentity
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

// isInterruptCode reports whether c is one an executor emits when its context is
// cancelled mid-call (files map a cancel to INTERNAL and a deadline to
// DEADLINE_EXCEEDED; process/terminal map either to DEADLINE_EXCEEDED).
func isInterruptCode(c api.ErrorCode) bool {
	return c == api.CodeDeadlineExceeded || c == api.CodeInternal
}

func pausedMeta() api.ResultMeta {
	return api.ResultMeta{Code: api.CodePaused, Message: "root helper is paused", Retryable: true}
}

func closedMeta() api.ResultMeta {
	return api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "root helper is shut down", Retryable: false}
}

func queuedCancelMeta(ctx context.Context) api.ResultMeta {
	return api.ResultMeta{
		Code:      api.CodeDeadlineExceeded,
		Message:   "cancelled while queued for an execution slot: " + ctx.Err().Error(),
		Retryable: true,
	}
}

// ---------------------------------------------------------------------------
// Typed per-tool methods (each takes the shared semaphore)
// ---------------------------------------------------------------------------

func (h *Helper) terminal(ctx context.Context, in api.TerminalInput) api.TerminalOutput {
	return guard(h, ctx,
		func(o *api.TerminalOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.TerminalOutput {
			out, _ := h.process.Terminal(mctx, in)
			return out
		})
}

func (h *Helper) processTool(ctx context.Context, in api.ProcessInput) api.ProcessOutput {
	return guard(h, ctx,
		func(o *api.ProcessOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.ProcessOutput {
			out, _ := h.process.Process(mctx, in)
			h.trackProcess(in, out)
			return out
		})
}

func (h *Helper) readFile(ctx context.Context, in api.ReadFileInput) api.ReadFileOutput {
	return guard(h, ctx,
		func(o *api.ReadFileOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.ReadFileOutput {
			return h.files.Read(mctx, in)
		})
}

func (h *Helper) searchFiles(ctx context.Context, in api.SearchFilesInput) api.SearchFilesOutput {
	return guard(h, ctx,
		func(o *api.SearchFilesOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.SearchFilesOutput {
			return h.files.Search(mctx, in)
		})
}

func (h *Helper) writeFile(ctx context.Context, in api.WriteFileInput) api.WriteFileOutput {
	return guard(h, ctx,
		func(o *api.WriteFileOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.WriteFileOutput {
			return h.files.Write(mctx, in)
		})
}

func (h *Helper) patch(ctx context.Context, in api.PatchInput) api.PatchOutput {
	return guard(h, ctx,
		func(o *api.PatchOutput) *api.ResultMeta { return &o.ResultMeta },
		func(mctx context.Context) api.PatchOutput {
			return h.files.Patch(mctx, in)
		})
}

// trackProcess records or forgets a long-running root process id as
// start/terminate calls succeed, so a later pause can terminate its group. A
// process that starts while (or exactly as) the helper pauses is terminated
// immediately so it cannot outlive the pause: because both this check and
// Pause's snapshot run under h.mu, a new process is either captured by Pause's
// snapshot or sees paused==true here and self-terminates.
func (h *Helper) trackProcess(in api.ProcessInput, out api.ProcessOutput) {
	if in.Action == api.ProcessStart {
		if out.ResultMeta.Code != "" || out.ProcessID == "" {
			return
		}
		h.mu.Lock()
		paused := h.paused || h.closed
		if !paused {
			h.procIDs[out.ProcessID] = struct{}{}
		}
		h.mu.Unlock()
		if paused {
			_, _ = h.process.Process(context.Background(), api.ProcessInput{
				Action:    api.ProcessTerminate,
				ProcessID: out.ProcessID,
			})
		}
		return
	}
	if out.ProcessID != "" && !out.Running {
		h.mu.Lock()
		delete(h.procIDs, out.ProcessID)
		h.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Call dispatch
// ---------------------------------------------------------------------------

// Call is the helper's single entry point, shaped to be driven by an
// rpc.Handler: it receives the tool name, its raw JSON arguments, and the
// caller's raw Authorization header value (or "" if none was sent).
//
// The check order is a security invariant:
//
//  1. Reject a non-OS tool (computer_use or an unknown name) BEFORE any
//     authentication. computer_use -> FORBIDDEN, unknown -> INVALID_ARGUMENT.
//     A non-OS tool never triggers a bearer check or reaches an executor.
//  2. Verify the raw admin bearer LOCALLY with the helper's constant-time
//     Verifier, requiring class admin. Any failure returns here; no executor is
//     reached.
//  3. Only then dispatch to the six OS tools under the pause/concurrency/
//     identity discipline.
//
// It never returns a Go error for a tool-level or auth-level failure -- those
// come back in-band via ResultMeta.Code. A non-nil error is reserved for an
// internal marshalling failure.
func (h *Helper) Call(ctx context.Context, tool string, args json.RawMessage, authHeader string) (*mcp.CallToolResult, error) {
	// 1. Reject non-OS tools before any authentication or dispatch.
	if !isOSTool(tool) {
		if tool == api.ToolComputerUse {
			return toResult(api.ResultMeta{
				Code:    api.CodeForbidden,
				Message: "computer_use is not served by the root helper",
			})
		}
		return toResult(api.ResultMeta{
			Code:    api.CodeInvalidArgument,
			Message: fmt.Sprintf("unknown tool %q", tool),
		})
	}

	// 2. Re-verify the admin bearer locally. No dispatch path skips this.
	if err := h.verifyAdmin(authHeader); err != nil {
		return toResult(authErrorMeta(err))
	}

	// 3. Dispatch to the OS tool.
	switch tool {
	case api.ToolTerminal:
		return dispatchCall(args, func(in api.TerminalInput) any {
			return h.terminal(ctx, in)
		})
	case api.ToolProcess:
		return dispatchCall(args, func(in api.ProcessInput) any {
			return h.processTool(ctx, in)
		})
	case api.ToolReadFile:
		return dispatchCall(args, func(in api.ReadFileInput) any {
			return h.readFile(ctx, in)
		})
	case api.ToolSearchFiles:
		return dispatchCall(args, func(in api.SearchFilesInput) any {
			return h.searchFiles(ctx, in)
		})
	case api.ToolWriteFile:
		return dispatchCall(args, func(in api.WriteFileInput) any {
			return h.writeFile(ctx, in)
		})
	case api.ToolPatch:
		return dispatchCall(args, func(in api.PatchInput) any {
			return h.patch(ctx, in)
		})
	default:
		// Unreachable: isOSTool already gated the tool name above.
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
		return nil, fmt.Errorf("roothelper: marshal tool result: %w", err)
	}
	raw := json.RawMessage(data)
	return &mcp.CallToolResult{
		StructuredContent: raw,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil
}
