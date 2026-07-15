// Package cua's adapter.go turns the one unified computer_use action defined
// by agent/internal/api into the matching Cua MCP tool call(s), and turns
// Cua's response back into api.ComputerUseOutput. It is the single seat for
// computer control: every computer_use call funnels through Adapter.seat, a
// ctx-aware semaphore, so concurrent callers queue rather than interleave
// against the one Cua child.
package cua

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// Cua MCP tool names the adapter calls, kept as constants so tests assert on
// them without repeating string literals throughout.
const (
	toolGetDesktopState      = "get_desktop_state"
	toolGetWindowState       = "get_window_state"
	toolGetAccessibilityTree = "get_accessibility_tree"
	toolSetConfig            = "set_config"
	toolClick                = "click"
	toolDoubleClick          = "double_click"
	toolDrag                 = "drag"
	toolScroll               = "scroll"
	toolTypeText             = "type_text"
	toolPressKey             = "press_key"
	toolHotkey               = "hotkey"
	toolListWindows          = "list_windows"
	toolBringToFront         = "bring_to_front"
)

// deliveryForeground is the only delivery_mode the adapter ever sends. Every
// state-changing action sets it unconditionally: ComputerUseInput has no
// delivery-mode field at all, so there is no way for a caller to request
// anything else, and the adapter never leaves it unset.
const deliveryForeground = "foreground"

// maxWaitDuration caps the wait action's local timer. wait never calls Cua.
// It is a var, not a const, purely so tests can shrink it and observe the
// cap being applied without a real 30-second sleep.
var maxWaitDuration = 30 * time.Second

// Caller is the seam the Adapter depends on: the piece of *Client's surface
// it needs. *Client satisfies it; tests substitute a fake so mapping,
// serialization, and error handling can be verified without spawning a real
// cua-driver process.
type Caller interface {
	Call(ctx context.Context, name string, arguments any) (*mcp.CallToolResult, error)
}

// Adapter maps computer_use calls onto a Caller. seat is the single seat: a
// capacity-1 channel semaphore held for the full duration of a ComputerUse
// call, including any multi-call sequence a single action needs (for example
// the capture_scope flip before a desktop capture), so those sub-calls are
// never split by a concurrent computer_use request. Unlike a plain
// sync.Mutex, acquiring it is ctx-aware: a waiter queued behind another
// caller can be preempted by its own ctx instead of dispatching a mutation
// with an already-dead ctx once it finally gets the seat.
type Adapter struct {
	caller Caller

	seat chan struct{}
}

// NewAdapter returns an Adapter that maps computer_use calls onto caller.
func NewAdapter(caller Caller) *Adapter {
	return &Adapter{caller: caller, seat: make(chan struct{}, 1)}
}

// ComputerUse maps in onto the Cua call(s) it requires and returns the
// unified result. It re-validates in with api.ComputerUseInput.Validate so
// the adapter is safe to call directly, but the mapping below still assumes
// a validated input's field combinations.
func (a *Adapter) ComputerUse(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	if err := in.Validate(); err != nil {
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeInvalidArgument, err.Error(), false)}, nil
	}

	if in.Action == api.ActionWait {
		// wait is a pure local timer; it never touches Cua, so it does not
		// need (and must not hold) the single seat other actions share.
		return a.wait(ctx, in)
	}

	select {
	case a.seat <- struct{}{}:
	case <-ctx.Done():
		// Queued behind another caller and preempted by our own ctx: nothing
		// was ever dispatched to Cua, so this is unambiguously safe to retry.
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeDeadlineExceeded, "computer_use: cancelled while waiting for the computer-use seat: "+ctx.Err().Error(), true)}, nil
	}
	defer func() { <-a.seat }()

	// Belt-and-suspenders: even after winning the seat, ctx may have expired
	// in the gap between the select above unblocking and here. Check again
	// before dispatching anything so an abandoned mutation is never written
	// to the transport with an already-dead ctx.
	if err := ctx.Err(); err != nil {
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeDeadlineExceeded, "computer_use: ctx expired after acquiring the computer-use seat, before dispatch: "+err.Error(), true)}, nil
	}

	switch in.Action {
	case api.ActionCapture:
		return a.capture(ctx, in)
	case api.ActionAccessibility:
		return a.accessibility(ctx, in)
	case api.ActionClick:
		return a.pointAction(ctx, toolClick, in)
	case api.ActionDoubleClick:
		return a.pointAction(ctx, toolDoubleClick, in)
	case api.ActionDrag:
		return a.drag(ctx, in)
	case api.ActionScroll:
		return a.scroll(ctx, in)
	case api.ActionType:
		return a.typeText(ctx, in)
	case api.ActionKey:
		return a.key(ctx, in)
	case api.ActionListApplications:
		return a.listApplications(ctx, in)
	case api.ActionFocusApplication:
		return a.focusApplication(ctx, in)
	default:
		// Unreachable given Validate above; kept so a future action added to
		// the api package without a matching adapter branch fails loudly
		// instead of silently doing nothing.
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeInvalidArgument, fmt.Sprintf("action %q is not mapped to a Cua call", in.Action), false)}, nil
	}
}

// ---------------------------------------------------------------------------
// capture / accessibility
// ---------------------------------------------------------------------------

func (a *Adapter) capture(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	if in.Scope == api.ScopeWindow {
		result, rmeta := a.invoke(ctx, toolGetWindowState, windowArgs(in), true)
		if rmeta.Code != "" {
			return api.ComputerUseOutput{ResultMeta: rmeta}, nil
		}
		return api.ComputerUseOutput{ResultMeta: rmeta, ImageBase64: extractImage(result)}, nil
	}

	// Desktop-wide capture: Cua defaults to window-scoped captures, so flip
	// capture_scope to desktop before reading the desktop state, then flip it
	// back. capture_scope is process-global cua-driver state (it outlives
	// this one call), so leaving it on "desktop" would silently change the
	// behavior of a later window-scoped call in this seat or in the next
	// ComputerUse call entirely. The restore is deferred so it runs on BOTH
	// the success and failure path of get_desktop_state: a failed desktop
	// read with an otherwise-live session must not leave capture_scope
	// flipped, or the next window-scoped call gets corrupted. It is
	// best-effort either way: it never overrides get_desktop_state's own
	// result (success or failure), since that result is valid regardless of
	// whether the restore itself lands.
	if _, rmeta := a.invoke(ctx, toolSetConfig, map[string]any{"capture_scope": "desktop"}, true); rmeta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: rmeta}, nil
	}
	defer func() {
		_, _ = a.invoke(ctx, toolSetConfig, map[string]any{"capture_scope": "window"}, true)
	}()
	result, rmeta := a.invoke(ctx, toolGetDesktopState, map[string]any{}, true)
	if rmeta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: rmeta}, nil
	}
	return api.ComputerUseOutput{ResultMeta: rmeta, ImageBase64: extractImage(result)}, nil
}

func (a *Adapter) accessibility(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	if in.Scope == api.ScopeWindow {
		args := windowArgs(in)
		args["include_screenshot"] = false
		result, rmeta := a.invoke(ctx, toolGetWindowState, args, true)
		if rmeta.Code != "" {
			return api.ComputerUseOutput{ResultMeta: rmeta}, nil
		}
		elements, err := decodeElements(result)
		if err != nil {
			return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeInternal, err.Error(), false)}, nil
		}
		return api.ComputerUseOutput{ResultMeta: rmeta, Elements: elements}, nil
	}

	args := map[string]any{}
	if in.Query != "" {
		args["query"] = in.Query
	}
	if in.MaxElements != nil {
		args["max_elements"] = *in.MaxElements
	}
	if in.MaxDepth != nil {
		args["max_depth"] = *in.MaxDepth
	}
	result, rmeta := a.invoke(ctx, toolGetAccessibilityTree, args, true)
	if rmeta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: rmeta}, nil
	}
	elements, err := decodeElements(result)
	if err != nil {
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeInternal, err.Error(), false)}, nil
	}
	return api.ComputerUseOutput{ResultMeta: rmeta, Elements: elements}, nil
}

// ---------------------------------------------------------------------------
// mutations: click / double_click / drag / scroll / type / key
// ---------------------------------------------------------------------------

func (a *Adapter) pointAction(ctx context.Context, tool string, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	args := windowArgs(in)
	args["delivery_mode"] = deliveryForeground
	if in.ElementIndex != nil {
		args["element_index"] = *in.ElementIndex
	}
	if in.X != nil {
		args["x"] = *in.X
	}
	if in.Y != nil {
		args["y"] = *in.Y
	}
	if in.Button != "" {
		args["button"] = string(in.Button)
	}
	_, rmeta := a.invoke(ctx, tool, args, false)
	return api.ComputerUseOutput{ResultMeta: rmeta}, nil
}

func (a *Adapter) drag(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	args := windowArgs(in)
	args["delivery_mode"] = deliveryForeground
	if in.FromX != nil {
		args["from_x"] = *in.FromX
	}
	if in.FromY != nil {
		args["from_y"] = *in.FromY
	}
	if in.ToX != nil {
		args["to_x"] = *in.ToX
	}
	if in.ToY != nil {
		args["to_y"] = *in.ToY
	}
	if in.Button != "" {
		args["button"] = string(in.Button)
	}
	_, rmeta := a.invoke(ctx, toolDrag, args, false)
	return api.ComputerUseOutput{ResultMeta: rmeta}, nil
}

func (a *Adapter) scroll(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	args := windowArgs(in)
	args["delivery_mode"] = deliveryForeground
	if in.X != nil {
		args["x"] = *in.X
	}
	if in.Y != nil {
		args["y"] = *in.Y
	}
	if in.Direction != "" {
		args["direction"] = string(in.Direction)
	}
	if in.Amount != nil {
		args["amount"] = *in.Amount
	}
	_, rmeta := a.invoke(ctx, toolScroll, args, false)
	return api.ComputerUseOutput{ResultMeta: rmeta}, nil
}

func (a *Adapter) typeText(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	args := windowArgs(in)
	args["delivery_mode"] = deliveryForeground
	args["text"] = in.Text
	if in.ElementIndex != nil {
		args["element_index"] = *in.ElementIndex
	}
	_, rmeta := a.invoke(ctx, toolTypeText, args, false)
	return api.ComputerUseOutput{ResultMeta: rmeta}, nil
}

func (a *Adapter) key(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	args := windowArgs(in)
	args["delivery_mode"] = deliveryForeground
	args["key"] = in.Key

	tool := toolPressKey
	if len(in.Modifiers) > 0 {
		// A chord (key held with modifiers) goes through hotkey instead of
		// press_key.
		tool = toolHotkey
		modifiers := make([]string, len(in.Modifiers))
		copy(modifiers, in.Modifiers)
		args["modifiers"] = modifiers
	}

	_, rmeta := a.invoke(ctx, tool, args, false)
	return api.ComputerUseOutput{ResultMeta: rmeta}, nil
}

// ---------------------------------------------------------------------------
// list_applications / focus_application
// ---------------------------------------------------------------------------

func (a *Adapter) listApplications(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	result, rmeta := a.invoke(ctx, toolListWindows, map[string]any{}, true)
	if rmeta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: rmeta}, nil
	}
	windows, err := decodeWindows(result)
	if err != nil {
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeInternal, err.Error(), false)}, nil
	}
	return api.ComputerUseOutput{ResultMeta: rmeta, Applications: groupApplications(windows, in.Query)}, nil
}

func (a *Adapter) focusApplication(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	// bring_to_front is idempotent from the caller's point of view (bringing
	// an already-front window to the front again is a no-op), so unlike
	// click/type/drag/scroll/key it carries none of the double-execution
	// hazard the mutation classification exists to guard against.
	_, rmeta := a.invoke(ctx, toolBringToFront, windowArgs(in), true)
	if rmeta.Code != "" {
		return api.ComputerUseOutput{ResultMeta: rmeta}, nil
	}
	out := api.ComputerUseOutput{ResultMeta: rmeta}
	if in.WindowID != nil {
		out.WindowID = *in.WindowID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

func (a *Adapter) wait(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	d := time.Duration(*in.DurationMs) * time.Millisecond
	if d > maxWaitDuration {
		d = maxWaitDuration
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return api.ComputerUseOutput{ResultMeta: metaFor(api.CodeDeadlineExceeded, "computer_use: wait cancelled: "+ctx.Err().Error(), true)}, nil
	case <-timer.C:
		return api.ComputerUseOutput{}, nil
	}
}

// ---------------------------------------------------------------------------
// call plumbing
// ---------------------------------------------------------------------------

// invoke issues one Cua call and classifies the outcome. On success it
// returns a zero-value ResultMeta (Code == ""); on failure the result is
// nil and the returned ResultMeta.Code is non-empty. invoke is called
// exactly once per logical sub-step: neither invoke nor its callers ever
// retry a call themselves, so a mutation that fails mid-flight (a
// disconnect, a cancellation) is reported once and never replayed.
//
// readOnly tells classifyErr whether name is one of the actions that never
// mutates on-screen state (capture, accessibility, list_applications,
// focus_application): for those it is always safe to advertise Retryable,
// since replaying them cannot double-execute a user-visible effect. Mutation
// sub-calls (click, double_click, drag, scroll, type_text, press_key/hotkey)
// pass readOnly=false, which narrows Retryable to only the case classifyErr
// can prove the call never reached the transport.
func (a *Adapter) invoke(ctx context.Context, name string, args map[string]any, readOnly bool) (*mcp.CallToolResult, api.ResultMeta) {
	result, err := a.caller.Call(ctx, name, args)
	if err != nil {
		return nil, classifyErr(err, readOnly)
	}
	if result != nil && result.IsError {
		return result, metaFor(api.CodeInternal, fmt.Sprintf("%s: %s", name, contentText(result)), false)
	}
	return result, api.ResultMeta{}
}

// classifyErr maps a Caller.Call error to the stable error code the brief
// requires: context cancellation/timeout becomes DEADLINE_EXCEEDED, and
// every other transport-level failure (child absent, display gone, a
// disconnect detected by the client, an in-flight reconnect not yet done)
// becomes SESSION_UNAVAILABLE.
//
// Retryable is not simply "always true": for a mutation (readOnly=false) the
// adapter cannot tell "the call never reached the transport" (safe to
// retry) apart from "the call was dispatched and then the child died or ctx
// expired mid-flight" (a retry could double-execute a click/type/drag/etc.)
// except in one provable case — *Client.Call's pre-transport fast path
// returns ErrDisconnected without ever touching the transport when it is
// already disconnected. So a mutation is Retryable only when err is
// ErrDisconnected; every other mutation failure (EOF/disconnect mid-call,
// ctx cancel/deadline observed after dispatch) is not. Read-only actions
// have no such hazard: replaying a capture/accessibility/list/focus read
// cannot double-execute a user-visible effect, so they stay Retryable
// regardless of err.
func classifyErr(err error, readOnly bool) api.ResultMeta {
	retryable := readOnly || errors.Is(err, ErrDisconnected)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return metaFor(api.CodeDeadlineExceeded, "computer_use: deadline exceeded: "+err.Error(), retryable)
	}
	return metaFor(api.CodeSessionUnavailable, "computer_use: Cua session unavailable: "+err.Error(), retryable)
}

func metaFor(code api.ErrorCode, message string, retryable bool) api.ResultMeta {
	return api.ResultMeta{Code: code, Message: message, Retryable: retryable}
}

// windowArgs returns the pid/window_id args common to every window-targeted
// Cua call, omitting whichever of the two in does not set.
func windowArgs(in api.ComputerUseInput) map[string]any {
	args := map[string]any{}
	if in.PID != nil {
		args["pid"] = *in.PID
	}
	if in.WindowID != nil {
		args["window_id"] = *in.WindowID
	}
	return args
}

// ---------------------------------------------------------------------------
// response decoding
// ---------------------------------------------------------------------------

// extractImage returns the base64 encoding of the first image content item
// in result, or "" if there is none. This is how the adapter preserves a
// capture's screenshot: Cua's ImageContent.Data decodes to raw bytes, and
// api.ComputerUseOutput.ImageBase64 wants base64 text.
func extractImage(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	for _, c := range result.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			return base64.StdEncoding.EncodeToString(img.Data)
		}
	}
	return ""
}

// contentText concatenates every text content item in result, used to
// surface a Cua tool-reported error message.
func contentText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range result.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// decodeStructured unmarshals result.StructuredContent into out.
func decodeStructured(result *mcp.CallToolResult, out any) error {
	if result == nil || result.StructuredContent == nil {
		return errors.New("cua: no structured content in tool result")
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("cua: marshalling structured content: %w", err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		return fmt.Errorf("cua: decoding structured content: %w", err)
	}
	return nil
}

// cuaFrame is a Cua element's bounding box, as returned by get_window_state
// and get_accessibility_tree.
type cuaFrame struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// cuaElement is one accessibility element as Cua reports it.
type cuaElement struct {
	ElementIndex int       `json:"element_index"`
	ParentIndex  *int      `json:"parent_index,omitempty"`
	Role         string    `json:"role,omitempty"`
	Label        string    `json:"label,omitempty"`
	Frame        *cuaFrame `json:"frame,omitempty"`
}

type cuaElementsOutput struct {
	Elements []cuaElement `json:"elements"`
}

// decodeElements decodes the flattened element list returned by both
// get_window_state and get_accessibility_tree into
// []api.AccessibilityElement.
func decodeElements(result *mcp.CallToolResult) ([]api.AccessibilityElement, error) {
	var out cuaElementsOutput
	if err := decodeStructured(result, &out); err != nil {
		return nil, err
	}
	elements := make([]api.AccessibilityElement, 0, len(out.Elements))
	for _, e := range out.Elements {
		el := api.AccessibilityElement{
			Index:       e.ElementIndex,
			ParentIndex: e.ParentIndex,
			Role:        e.Role,
			Name:        e.Label,
		}
		if e.Frame != nil {
			el.X, el.Y, el.Width, el.Height = e.Frame.X, e.Frame.Y, e.Frame.W, e.Frame.H
		}
		elements = append(elements, el)
	}
	return elements, nil
}

// cuaWindow is one window as list_windows reports it.
type cuaWindow struct {
	WindowID int64  `json:"window_id"`
	PID      int    `json:"pid"`
	AppName  string `json:"app_name,omitempty"`
	Title    string `json:"title,omitempty"`
}

type cuaWindowsOutput struct {
	Windows []cuaWindow `json:"windows"`
}

func decodeWindows(result *mcp.CallToolResult) ([]cuaWindow, error) {
	var out cuaWindowsOutput
	if err := decodeStructured(result, &out); err != nil {
		return nil, err
	}
	return out.Windows, nil
}

// groupApplications collapses list_windows' one-entry-per-window list into
// one api.ApplicationInfo per distinct (pid, app name), optionally filtered
// by a case-insensitive substring match against the app name or title. The
// representative window_id for each application is its first window in
// list_windows' order.
func groupApplications(windows []cuaWindow, query string) []api.ApplicationInfo {
	q := strings.ToLower(strings.TrimSpace(query))

	type key struct {
		pid  int
		name string
	}
	seen := make(map[key]bool, len(windows))

	var apps []api.ApplicationInfo
	for _, w := range windows {
		if q != "" &&
			!strings.Contains(strings.ToLower(w.AppName), q) &&
			!strings.Contains(strings.ToLower(w.Title), q) {
			continue
		}
		k := key{pid: w.PID, name: w.AppName}
		if seen[k] {
			continue
		}
		seen[k] = true
		apps = append(apps, api.ApplicationInfo{
			PID:      w.PID,
			Name:     w.AppName,
			WindowID: w.WindowID,
		})
	}
	return apps
}
