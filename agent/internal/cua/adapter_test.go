package cua

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ---------------------------------------------------------------------------
// fake Cua
// ---------------------------------------------------------------------------

// recordedCall is one Call the fakeCaller observed.
type recordedCall struct {
	name string
	args map[string]any
}

// fakeCaller is a hermetic stand-in for *Client: it implements the Caller
// seam the Adapter depends on, records every call it receives, tracks the
// maximum number of Call invocations that were ever concurrently in flight,
// and lets a test script the response (or error) per call via handler.
type fakeCaller struct {
	mu    sync.Mutex
	calls []recordedCall

	active    int32
	maxActive int32

	handler func(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
}

func (f *fakeCaller) Call(ctx context.Context, name string, arguments any) (*mcp.CallToolResult, error) {
	args, _ := arguments.(map[string]any)

	cur := atomic.AddInt32(&f.active, 1)
	defer atomic.AddInt32(&f.active, -1)
	for {
		m := atomic.LoadInt32(&f.maxActive)
		if cur <= m || atomic.CompareAndSwapInt32(&f.maxActive, m, cur) {
			break
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{name: name, args: args})
	f.mu.Unlock()

	if f.handler != nil {
		return f.handler(ctx, name, args)
	}
	return &mcp.CallToolResult{}, nil
}

func (f *fakeCaller) Calls() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// okResult returns a successful, empty CallToolResult.
func okResult() (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// mapping: every action
// ---------------------------------------------------------------------------

func TestComputerUseMapsEveryActionToTheRightCuaCall(t *testing.T) {
	tests := []struct {
		name  string
		input api.ComputerUseInput
		want  []recordedCall
	}{
		{
			name:  "capture desktop (default scope)",
			input: api.ComputerUseInput{Action: api.ActionCapture},
			want: []recordedCall{
				{name: toolSetConfig, args: map[string]any{"capture_scope": "desktop"}},
				{name: toolGetDesktopState, args: map[string]any{}},
				{name: toolSetConfig, args: map[string]any{"capture_scope": "window"}},
			},
		},
		{
			name:  "capture desktop (explicit screen scope)",
			input: api.ComputerUseInput{Action: api.ActionCapture, Scope: api.ScopeScreen},
			want: []recordedCall{
				{name: toolSetConfig, args: map[string]any{"capture_scope": "desktop"}},
				{name: toolGetDesktopState, args: map[string]any{}},
				{name: toolSetConfig, args: map[string]any{"capture_scope": "window"}},
			},
		},
		{
			name: "capture window",
			input: api.ComputerUseInput{
				Action: api.ActionCapture, Scope: api.ScopeWindow, PID: intPtr(7),
			},
			want: []recordedCall{
				{name: toolGetWindowState, args: map[string]any{"pid": 7}},
			},
		},
		{
			name: "accessibility desktop",
			input: api.ComputerUseInput{
				Action: api.ActionAccessibility,
				Query:  "foo", MaxElements: intPtr(5), MaxDepth: intPtr(3),
			},
			want: []recordedCall{
				{name: toolGetAccessibilityTree, args: map[string]any{
					"query": "foo", "max_elements": 5, "max_depth": 3,
				}},
			},
		},
		{
			name: "accessibility window",
			input: api.ComputerUseInput{
				Action: api.ActionAccessibility, Scope: api.ScopeWindow, WindowID: int64Ptr(42),
			},
			want: []recordedCall{
				{name: toolGetWindowState, args: map[string]any{
					"window_id": int64(42), "include_screenshot": false,
				}},
			},
		},
		{
			name: "click by coordinate",
			input: api.ComputerUseInput{
				Action: api.ActionClick, X: intPtr(10), Y: intPtr(20), Button: api.ButtonLeft,
			},
			want: []recordedCall{
				{name: toolClick, args: map[string]any{
					"x": 10, "y": 20, "button": "left", "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name: "double_click by element",
			input: api.ComputerUseInput{
				Action: api.ActionDoubleClick, PID: intPtr(5), ElementIndex: intPtr(3),
			},
			want: []recordedCall{
				{name: toolDoubleClick, args: map[string]any{
					"pid": 5, "element_index": 3, "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name: "drag",
			input: api.ComputerUseInput{
				Action: api.ActionDrag,
				FromX:  intPtr(1), FromY: intPtr(2), ToX: intPtr(3), ToY: intPtr(4),
			},
			want: []recordedCall{
				{name: toolDrag, args: map[string]any{
					"from_x": 1, "from_y": 2, "to_x": 3, "to_y": 4, "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name: "scroll",
			input: api.ComputerUseInput{
				Action: api.ActionScroll, Direction: api.DirectionDown, Amount: intPtr(5),
				X: intPtr(1), Y: intPtr(2),
			},
			want: []recordedCall{
				{name: toolScroll, args: map[string]any{
					"direction": "down", "amount": 5, "x": 1, "y": 2, "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name: "type",
			input: api.ComputerUseInput{
				Action: api.ActionType, Text: "hi", ElementIndex: intPtr(2),
			},
			want: []recordedCall{
				{name: toolTypeText, args: map[string]any{
					"text": "hi", "element_index": 2, "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name:  "key: single key uses press_key",
			input: api.ComputerUseInput{Action: api.ActionKey, Key: "Return"},
			want: []recordedCall{
				{name: toolPressKey, args: map[string]any{
					"key": "Return", "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name: "key: chord with modifiers uses hotkey",
			input: api.ComputerUseInput{
				Action: api.ActionKey, Key: "t", Modifiers: []string{"ctrl", "shift"},
			},
			want: []recordedCall{
				{name: toolHotkey, args: map[string]any{
					"key": "t", "modifiers": []string{"ctrl", "shift"}, "delivery_mode": deliveryForeground,
				}},
			},
		},
		{
			name:  "list_applications",
			input: api.ComputerUseInput{Action: api.ActionListApplications},
			want: []recordedCall{
				{name: toolListWindows, args: map[string]any{}},
			},
		},
		{
			name: "focus_application",
			input: api.ComputerUseInput{
				Action: api.ActionFocusApplication, WindowID: int64Ptr(99),
			},
			want: []recordedCall{
				{name: toolBringToFront, args: map[string]any{"window_id": int64(99)}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCaller{}
			adapter := NewAdapter(fake)

			// Only the call shape (tool name + args) is under test here;
			// response decoding is covered separately by the
			// preservation/decode tests below, so the fake's default empty
			// result (no StructuredContent) is fine even though a couple of
			// actions then report a decode failure.
			if _, err := adapter.ComputerUse(t.Context(), tt.input); err != nil {
				t.Fatalf("ComputerUse returned an error: %v", err)
			}

			got := fake.Calls()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Cua calls = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestComputerUseWaitNeverCallsCua(t *testing.T) {
	fake := &fakeCaller{}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionWait, DurationMs: intPtr(1),
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("wait failed: code=%s message=%s", out.Code, out.Message)
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("wait issued %d Cua calls, want 0", n)
	}
}

func TestComputerUseWaitCapsAt30SecondsEquivalent(t *testing.T) {
	original := maxWaitDuration
	maxWaitDuration = 20 * time.Millisecond
	t.Cleanup(func() { maxWaitDuration = original })

	fake := &fakeCaller{}
	adapter := NewAdapter(fake)

	start := time.Now()
	// Ask for a wait far longer than the (shrunk-for-test) cap; the adapter
	// must return once the cap elapses, not once the requested duration
	// elapses.
	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionWait, DurationMs: intPtr(1_000_000),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("wait failed: code=%s message=%s", out.Code, out.Message)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("wait took %s, want it capped near %s", elapsed, maxWaitDuration)
	}
}

func TestComputerUseWaitCancellationIsDeadlineExceeded(t *testing.T) {
	fake := &fakeCaller{}
	adapter := NewAdapter(fake)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	out, err := adapter.ComputerUse(ctx, api.ComputerUseInput{
		Action: api.ActionWait, DurationMs: intPtr(5_000),
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("wait cancellation code = %q, want %q", out.Code, api.CodeDeadlineExceeded)
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("cancelled wait issued %d Cua calls, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// foreground enforcement
// ---------------------------------------------------------------------------

func TestComputerUseAlwaysSetsForegroundDeliveryOnMutations(t *testing.T) {
	mutations := []api.ComputerUseInput{
		{Action: api.ActionClick, X: intPtr(1), Y: intPtr(1)},
		{Action: api.ActionDoubleClick, X: intPtr(1), Y: intPtr(1)},
		{Action: api.ActionDrag, FromX: intPtr(1), FromY: intPtr(1), ToX: intPtr(2), ToY: intPtr(2)},
		{Action: api.ActionScroll, Direction: api.DirectionUp},
		{Action: api.ActionType, Text: "x"},
		{Action: api.ActionKey, Key: "a"},
		{Action: api.ActionKey, Key: "a", Modifiers: []string{"ctrl"}},
	}

	for _, in := range mutations {
		t.Run(string(in.Action), func(t *testing.T) {
			fake := &fakeCaller{}
			adapter := NewAdapter(fake)

			if _, err := adapter.ComputerUse(t.Context(), in); err != nil {
				t.Fatalf("ComputerUse returned an error: %v", err)
			}

			calls := fake.Calls()
			if len(calls) != 1 {
				t.Fatalf("got %d Cua calls, want 1", len(calls))
			}
			if got := calls[0].args["delivery_mode"]; got != deliveryForeground {
				t.Fatalf("delivery_mode = %v, want %q", got, deliveryForeground)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// error mapping
// ---------------------------------------------------------------------------

func TestComputerUseChildAbsentIsSessionUnavailable(t *testing.T) {
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		return nil, errors.New("dial cua-driver: no such process")
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionClick, X: intPtr(1), Y: intPtr(1),
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeSessionUnavailable {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
	}
}

func TestComputerUseDisconnectDuringMutationIsNotReplayed(t *testing.T) {
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		return nil, ErrDisconnected
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionType, Text: "hello",
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeSessionUnavailable {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
	}
	if n := fake.callCount(); n != 1 {
		t.Fatalf("the failed mutation was sent %d times, want exactly 1 (no replay)", n)
	}
}

// TestComputerUseRetryableReflectsMutationHazard covers Fix I1: Retryable on
// a failed action must depend on whether a replay could double-execute a
// mutation, not just on the error code. A mutation is Retryable only when
// the failure provably predates dispatch (ErrDisconnected, *Client's
// pre-transport fast path); every other mutation failure, and every
// read-only-action failure regardless of cause, keeps its prior behavior.
func TestComputerUseRetryableReflectsMutationHazard(t *testing.T) {
	t.Run("mutation failing mid-flight is not retryable", func(t *testing.T) {
		fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
			// The fakeCaller records the call before invoking handler, so this
			// error models a failure discovered only after the call already
			// reached the transport (e.g. an EOF mid-response) -- exactly the
			// case a retry could double-execute.
			return nil, errors.New("connection reset by peer")
		}}
		adapter := NewAdapter(fake)

		out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
			Action: api.ActionClick, X: intPtr(1), Y: intPtr(1),
		})
		if err != nil {
			t.Fatalf("ComputerUse returned an error: %v", err)
		}
		if out.Code != api.CodeSessionUnavailable {
			t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
		}
		if out.Retryable {
			t.Fatal("mutation failing mid-flight must not be marked Retryable")
		}
		if n := fake.callCount(); n != 1 {
			t.Fatalf("mutation was sent %d times, want exactly 1 (no replay)", n)
		}
	})

	t.Run("mutation failing via pre-dispatch ErrDisconnected is retryable", func(t *testing.T) {
		fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
			// *Client.Call returns ErrDisconnected from its pre-transport fast
			// path without ever touching the transport, so this failure
			// provably predates dispatch.
			return nil, ErrDisconnected
		}}
		adapter := NewAdapter(fake)

		out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
			Action: api.ActionType, Text: "hello",
		})
		if err != nil {
			t.Fatalf("ComputerUse returned an error: %v", err)
		}
		if out.Code != api.CodeSessionUnavailable {
			t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
		}
		if !out.Retryable {
			t.Fatal("mutation failing via pre-dispatch ErrDisconnected must be marked Retryable")
		}
	})

	t.Run("read-only action failure stays retryable", func(t *testing.T) {
		fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
			return nil, errors.New("connection reset by peer")
		}}
		adapter := NewAdapter(fake)

		out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
			Action: api.ActionAccessibility,
		})
		if err != nil {
			t.Fatalf("ComputerUse returned an error: %v", err)
		}
		if out.Code != api.CodeSessionUnavailable {
			t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
		}
		if !out.Retryable {
			t.Fatal("read-only action failure must stay Retryable")
		}
	})
}

func TestComputerUseCancellationIsDeadlineExceeded(t *testing.T) {
	fake := &fakeCaller{handler: func(ctx context.Context, _ string, _ map[string]any) (*mcp.CallToolResult, error) {
		return nil, ctx.Err()
	}}
	adapter := NewAdapter(fake)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, err := adapter.ComputerUse(ctx, api.ComputerUseInput{
		Action: api.ActionClick, X: intPtr(1), Y: intPtr(1),
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeDeadlineExceeded)
	}
	// The ctx is already cancelled before ComputerUse is even called, so the
	// belt-and-suspenders check (Fix I2) must catch it before invoke ever
	// dispatches to Cua: 0 calls, not 1. Dispatching a mutation with an
	// already-dead ctx is exactly the hazard that check exists to close.
	if n := fake.callCount(); n != 0 {
		t.Fatalf("cancelled call issued %d Cua calls, want exactly 0 (never dispatched)", n)
	}
}

// ---------------------------------------------------------------------------
// response / image preservation
// ---------------------------------------------------------------------------

func TestComputerUseCapturePreservesScreenshot(t *testing.T) {
	imageBytes := []byte("not-really-a-png-but-good-enough-for-a-test")
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{MIMEType: "image/png", Data: imageBytes},
			},
		}, nil
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionCapture, Scope: api.ScopeWindow, PID: intPtr(1),
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("capture failed: code=%s message=%s", out.Code, out.Message)
	}
	want := base64.StdEncoding.EncodeToString(imageBytes)
	if out.ImageBase64 != want {
		t.Fatalf("ImageBase64 = %q, want %q", out.ImageBase64, want)
	}
}

func TestComputerUseDesktopCapturePreservesScreenshotAndRestoresScope(t *testing.T) {
	imageBytes := []byte("desktop-capture-bytes")
	fake := &fakeCaller{handler: func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolGetDesktopState {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: imageBytes}},
			}, nil
		}
		return okResult()
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{Action: api.ActionCapture})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("capture failed: code=%s message=%s", out.Code, out.Message)
	}
	want := base64.StdEncoding.EncodeToString(imageBytes)
	if out.ImageBase64 != want {
		t.Fatalf("ImageBase64 = %q, want %q", out.ImageBase64, want)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("got %d Cua calls, want 3 (set desktop, get, restore window): %#v", len(calls), calls)
	}
	if calls[2].name != toolSetConfig || calls[2].args["capture_scope"] != "window" {
		t.Fatalf("final call = %#v, want a set_config(capture_scope=window) restore", calls[2])
	}
}

func TestComputerUseDesktopCaptureRestoreFailureDoesNotOverrideSuccess(t *testing.T) {
	imageBytes := []byte("desktop-capture-bytes")
	restoreAttempted := false
	fake := &fakeCaller{handler: func(_ context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
		switch {
		case name == toolGetDesktopState:
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: imageBytes}},
			}, nil
		case name == toolSetConfig && args["capture_scope"] == "window":
			restoreAttempted = true
			return nil, errors.New("connection reset")
		default:
			return okResult()
		}
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{Action: api.ActionCapture})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("capture failed even though the screenshot itself succeeded: code=%s message=%s", out.Code, out.Message)
	}
	want := base64.StdEncoding.EncodeToString(imageBytes)
	if out.ImageBase64 != want {
		t.Fatalf("ImageBase64 = %q, want %q", out.ImageBase64, want)
	}
	if !restoreAttempted {
		t.Fatal("restore set_config was never attempted")
	}
}

// TestComputerUseDesktopCaptureFailureStillRestoresScope covers Fix M1: when
// get_desktop_state itself fails (not just the restore), capture_scope must
// still be flipped back to "window" on the way out. Without the restore, a
// live session would leak the flipped scope and corrupt the next
// window-scoped call in this seat or the next ComputerUse call entirely.
func TestComputerUseDesktopCaptureFailureStillRestoresScope(t *testing.T) {
	fake := &fakeCaller{handler: func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolGetDesktopState {
			return nil, errors.New("desktop read failed")
		}
		return okResult()
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{Action: api.ActionCapture})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	// The original failure must not be masked by the restore.
	if out.Code != api.CodeSessionUnavailable {
		t.Fatalf("code = %q, want %q (original get_desktop_state failure preserved)", out.Code, api.CodeSessionUnavailable)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("got %d Cua calls, want 3 (set desktop, failed get, restore window): %#v", len(calls), calls)
	}
	if calls[2].name != toolSetConfig || calls[2].args["capture_scope"] != "window" {
		t.Fatalf("final call = %#v, want a set_config(capture_scope=window) restore even on failure", calls[2])
	}
}

func TestComputerUseAccessibilityPreservesElements(t *testing.T) {
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{
				"elements": []map[string]any{
					{"element_index": 0, "role": "push button", "label": "OK",
						"frame": map[string]any{"x": 1, "y": 2, "w": 3, "h": 4}},
					{"element_index": 1, "parent_index": 0, "role": "text", "label": "Name"},
				},
			},
		}, nil
	}}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
		Action: api.ActionAccessibility,
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("accessibility failed: code=%s message=%s", out.Code, out.Message)
	}
	if len(out.Elements) != 2 {
		t.Fatalf("got %d elements, want 2", len(out.Elements))
	}
	if out.Elements[0].Name != "OK" || out.Elements[0].Role != "push button" {
		t.Fatalf("unexpected first element: %+v", out.Elements[0])
	}
	if out.Elements[0].X != 1 || out.Elements[0].Y != 2 || out.Elements[0].Width != 3 || out.Elements[0].Height != 4 {
		t.Fatalf("frame not preserved: %+v", out.Elements[0])
	}
	if out.Elements[1].ParentIndex == nil || *out.Elements[1].ParentIndex != 0 {
		t.Fatalf("parent index not preserved: %+v", out.Elements[1])
	}
}

// ---------------------------------------------------------------------------
// single-seat serialization
// ---------------------------------------------------------------------------

func TestComputerUseSerializesConcurrentCalls(t *testing.T) {
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		time.Sleep(15 * time.Millisecond)
		return okResult()
	}}
	adapter := NewAdapter(fake)

	const callers = 8
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
				Action: api.ActionClick, X: intPtr(n), Y: intPtr(n),
			})
			if err != nil {
				t.Errorf("ComputerUse returned an error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if fake.callCount() != callers {
		t.Fatalf("Cua saw %d calls, want %d", fake.callCount(), callers)
	}
	if max := atomic.LoadInt32(&fake.maxActive); max != 1 {
		t.Fatalf("maximum concurrent Cua calls = %d, want 1", max)
	}
}

// TestComputerUseCancelledWaiterNeverDispatches covers Fix I2: a caller
// queued behind another caller holding the seat must be preempted by its own
// ctx instead of blocking until the seat frees up and then dispatching a
// mutation whose ctx already expired.
func TestComputerUseCancelledWaiterNeverDispatches(t *testing.T) {
	holding := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		close(holding)
		<-release
		return okResult()
	}}
	adapter := NewAdapter(fake)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{
			Action: api.ActionClick, X: intPtr(1), Y: intPtr(1),
		})
		if err != nil {
			t.Errorf("holder ComputerUse returned an error: %v", err)
		}
	}()

	<-holding // the holder now has the seat and is inside its (blocked) Cua call

	waiterCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		// Cancel while the waiter is still queued behind the holder, which
		// won't release the seat until the test closes release below.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	out, err := adapter.ComputerUse(waiterCtx, api.ComputerUseInput{
		Action: api.ActionType, Text: "hello",
	})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeDeadlineExceeded)
	}
	// The waiter must never have dispatched to Cua: the only call so far is
	// the holder's still-in-flight one.
	if n := fake.callCount(); n != 1 {
		t.Fatalf("cancelled waiter caused %d Cua calls, want exactly 1 (the holder's; the waiter's mutation was never dispatched)", n)
	}

	close(release)
	wg.Wait()

	if n := fake.callCount(); n != 1 {
		t.Fatalf("after the holder released the seat, Cua saw %d calls, want 1 (the cancelled waiter still never dispatched)", n)
	}
}

// ---------------------------------------------------------------------------
// validation reuse
// ---------------------------------------------------------------------------

func TestComputerUseRejectsInvalidInputWithoutCallingCua(t *testing.T) {
	fake := &fakeCaller{}
	adapter := NewAdapter(fake)

	out, err := adapter.ComputerUse(t.Context(), api.ComputerUseInput{Action: "not_a_real_action"})
	if err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	if out.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeInvalidArgument)
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("invalid input issued %d Cua calls, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// pointer actions: pid resolved from window_id
// ---------------------------------------------------------------------------

// A window_id straight from list_applications must be enough to click with.
// The Cua driver's pointer tools reject a call that omits pid even when
// window_id already names the window, so the adapter resolves the owning pid
// from list_windows instead of leaking that pairing requirement into the tool
// contract (api.Validate accepts "pid or window_id").
func TestPointerActionsResolvePIDFromWindowID(t *testing.T) {
	const wantWindow, wantPID = int64(12582917), 2121

	windowsHandler := func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolListWindows {
			return &mcp.CallToolResult{StructuredContent: map[string]any{
				"windows": []any{
					map[string]any{"window_id": int64(4242), "pid": 111, "app_name": "other"},
					map[string]any{"window_id": wantWindow, "pid": wantPID, "app_name": "xterm-256color"},
				},
			}}, nil
		}
		return okResult()
	}

	for _, tt := range []struct {
		name  string
		input api.ComputerUseInput
		tool  string
	}{
		{"click", api.ComputerUseInput{Action: api.ActionClick, WindowID: int64Ptr(wantWindow), X: intPtr(10), Y: intPtr(20)}, toolClick},
		{"double_click", api.ComputerUseInput{Action: api.ActionDoubleClick, WindowID: int64Ptr(wantWindow), X: intPtr(10), Y: intPtr(20)}, toolDoubleClick},
		{"drag", api.ComputerUseInput{Action: api.ActionDrag, WindowID: int64Ptr(wantWindow), FromX: intPtr(1), FromY: intPtr(2), ToX: intPtr(3), ToY: intPtr(4)}, toolDrag},
		{"scroll", api.ComputerUseInput{Action: api.ActionScroll, WindowID: int64Ptr(wantWindow), Direction: api.DirectionDown, Amount: intPtr(2)}, toolScroll},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCaller{handler: windowsHandler}
			if _, err := NewAdapter(fake).ComputerUse(t.Context(), tt.input); err != nil {
				t.Fatalf("ComputerUse returned an error: %v", err)
			}
			var mutation *recordedCall
			sawList := false
			for i, c := range fake.Calls() {
				if c.name == toolListWindows {
					sawList = true
				}
				if c.name == tt.tool {
					mutation = &fake.Calls()[i]
				}
			}
			if !sawList {
				t.Fatalf("expected a %s lookup to resolve pid; calls=%#v", toolListWindows, fake.Calls())
			}
			if mutation == nil {
				t.Fatalf("%s was never called; calls=%#v", tt.tool, fake.Calls())
			}
			if got := mutation.args["pid"]; got != wantPID {
				t.Fatalf("%s pid = %#v, want %d (resolved from window_id)", tt.tool, got, wantPID)
			}
			if got := mutation.args["window_id"]; got != wantWindow {
				t.Fatalf("%s window_id = %#v, want %d", tt.tool, got, wantWindow)
			}
		})
	}
}

// When the caller already supplied pid there is nothing to resolve, so the
// adapter must not spend an extra round trip on list_windows.
func TestPointerActionsWithPIDSkipTheLookup(t *testing.T) {
	fake := &fakeCaller{}
	in := api.ComputerUseInput{Action: api.ActionClick, PID: intPtr(7), WindowID: int64Ptr(99), X: intPtr(1), Y: intPtr(2)}
	if _, err := NewAdapter(fake).ComputerUse(t.Context(), in); err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	for _, c := range fake.Calls() {
		if c.name == toolListWindows {
			t.Fatalf("unexpected %s lookup when pid was supplied; calls=%#v", toolListWindows, fake.Calls())
		}
	}
}

// A window_id the driver does not know about must not synthesise an error of
// our own: the call goes out unresolved so the driver's own message surfaces.
func TestPointerActionUnknownWindowIDStillDispatches(t *testing.T) {
	fake := &fakeCaller{handler: func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolListWindows {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"windows": []any{}}}, nil
		}
		return okResult()
	}}
	in := api.ComputerUseInput{Action: api.ActionClick, WindowID: int64Ptr(1234), X: intPtr(1), Y: intPtr(2)}
	if _, err := NewAdapter(fake).ComputerUse(t.Context(), in); err != nil {
		t.Fatalf("ComputerUse returned an error: %v", err)
	}
	var clicked *recordedCall
	for i, c := range fake.Calls() {
		if c.name == toolClick {
			clicked = &fake.Calls()[i]
		}
	}
	if clicked == nil {
		t.Fatalf("click was never dispatched; calls=%#v", fake.Calls())
	}
	if _, ok := clicked.args["pid"]; ok {
		t.Fatalf("pid must stay unset when the window is unknown; args=%#v", clicked.args)
	}
}
