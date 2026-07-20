package cua

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// browserWindows is a list_windows reply with one Chromium window and one
// window that is not a browser, so auto-resolution has something to reject.
func browserWindows() *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: map[string]any{
		"windows": []any{
			map[string]any{"window_id": 4242, "pid": 77, "app_name": "xterm-256color", "title": "shell"},
			map[string]any{"window_id": 1818, "pid": 99, "app_name": "Chromium", "title": "Kairos"},
		},
	}}
}

// jsReply builds the reply an execute_javascript call returns. The driver wraps
// and escapes the script's value, so the payload arrives as a JSON string
// containing JSON -- the shape unwrapJSON has to peel.
func jsReply(t *testing.T, payload map[string]any) *mcp.CallToolResult {
	t.Helper()
	inner, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	wrapped, err := json.Marshal(string(inner))
	if err != nil {
		t.Fatalf("wrap payload: %v", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(wrapped)}}}
}

// newBrowserCaller returns a caller that resolves windows and hands every
// execute_javascript call to js.
func newBrowserCaller(t *testing.T, js func(script string) *mcp.CallToolResult) *fakeCaller {
	t.Helper()
	return &fakeCaller{handler: func(_ context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
		switch name {
		case toolListWindows:
			return browserWindows(), nil
		case toolPage:
			if got := args["action"]; got != "execute_javascript" {
				t.Errorf("page action = %v, want execute_javascript (the only one implemented on the Linux backend)", got)
			}
			script, _ := args["javascript"].(string)
			return js(script), nil
		default:
			t.Errorf("unexpected tool call %q", name)
			return nil, nil
		}
	}}
}

// TestBrowserResolvesTheWindowWithoutAPid is the ergonomic promise of the
// tool: a caller names no window and no pid, and the call still lands on the
// browser with both filled in, because page needs them and the public contract
// should not.
func TestBrowserResolvesTheWindowWithoutAPid(t *testing.T) {
	caller := newBrowserCaller(t, func(string) *mcp.CallToolResult {
		return jsReply(t, map[string]any{"url": "https://kairos.io/", "title": "Kairos", "elements": []any{}})
	})
	a := NewAdapter(caller)

	out, err := a.Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot})
	if err != nil {
		t.Fatalf("Browser: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("code = %q, want success: %s", out.Code, out.Message)
	}
	if out.WindowID != 1818 {
		t.Fatalf("window_id = %d, want the Chromium window 1818", out.WindowID)
	}

	var page recordedCall
	for _, c := range caller.calls {
		if c.name == toolPage {
			page = c
		}
	}
	if page.args["pid"] != 99 {
		t.Errorf("page pid = %v, want the Chromium pid 99 resolved from list_windows", page.args["pid"])
	}
	if page.args["cdp_port"] != browserCDPPort {
		t.Errorf("page cdp_port = %v, want %d", page.args["cdp_port"], browserCDPPort)
	}
}

// TestBrowserWithoutABrowserWindow proves the dedicated error code: the desktop
// session is healthy, so reporting SESSION_UNAVAILABLE would send a caller
// looking in the wrong place.
func TestBrowserWithoutABrowserWindow(t *testing.T) {
	caller := &fakeCaller{handler: func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolListWindows {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"windows": []any{
				map[string]any{"window_id": 4242, "pid": 77, "app_name": "xterm-256color"},
			}}}, nil
		}
		t.Errorf("unexpected call %q: nothing should be dispatched without a browser", name)
		return nil, nil
	}}

	out, _ := NewAdapter(caller).Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot})
	if out.Code != api.CodeBrowserUnavailable {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeBrowserUnavailable)
	}
}

// TestBrowserAmbiguousWindowIsAnError: with two browsers open, picking one
// silently would act on a window the caller never named.
func TestBrowserAmbiguousWindowIsAnError(t *testing.T) {
	caller := &fakeCaller{handler: func(_ context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
		if name == toolListWindows {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"windows": []any{
				map[string]any{"window_id": 1, "pid": 10, "app_name": "Chromium", "title": "one"},
				map[string]any{"window_id": 2, "pid": 20, "app_name": "Firefox", "title": "two"},
			}}}, nil
		}
		t.Errorf("unexpected call %q: an ambiguous target must not be dispatched", name)
		return nil, nil
	}}

	out, _ := NewAdapter(caller).Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot})
	if out.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeInvalidArgument)
	}
	if !strings.Contains(out.Message, "window_id") {
		t.Errorf("message = %q, want it to tell the caller to pass window_id", out.Message)
	}
}

// TestBrowserSnapshotReturnsRefsAndBounds checks the decode path end to end,
// including the wrapped/escaped reply shape.
func TestBrowserSnapshotReturnsRefsAndBounds(t *testing.T) {
	caller := newBrowserCaller(t, func(script string) *mcp.CallToolResult {
		if !strings.Contains(script, "window.__hadronRefs=refs") {
			t.Error("snapshot script must leave the nodes in window.__hadronRefs for later ref actions")
		}
		if strings.Contains(script, "data-hadron") || strings.Contains(script, "setAttribute") {
			t.Error("snapshot must not stamp attributes onto the DOM: the page's own selectors and CSS can see them")
		}
		return jsReply(t, map[string]any{
			"url": "https://kairos.io/", "title": "Kairos",
			"elements": []any{
				map[string]any{"ref": "e1", "role": "a", "name": "Docs", "x": 261, "y": 203, "width": 40, "height": 18},
			},
			"truncated": true,
		})
	})

	out, _ := NewAdapter(caller).Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot})
	if out.Code != "" {
		t.Fatalf("code = %q, want success: %s", out.Code, out.Message)
	}
	if len(out.Elements) != 1 {
		t.Fatalf("elements = %d, want 1", len(out.Elements))
	}
	el := out.Elements[0]
	if el.Ref != "e1" || el.Name != "Docs" || el.X != 261 || el.Y != 203 {
		t.Fatalf("element = %+v, want ref e1 named Docs at (261,203)", el)
	}
	if !out.Truncated {
		t.Error("truncated was not reported back to the caller")
	}
}

// TestBrowserSnapshotBoundsElements: an unbounded request must still be
// bounded, so one call cannot return an arbitrarily large page.
func TestBrowserSnapshotBoundsElements(t *testing.T) {
	var script string
	caller := newBrowserCaller(t, func(s string) *mcp.CallToolResult {
		script = s
		return jsReply(t, map[string]any{"url": "u", "title": "t", "elements": []any{}})
	})
	a := NewAdapter(caller)

	huge := 999999
	if _, err := a.Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot, MaxElements: &huge}); err != nil {
		t.Fatalf("Browser: %v", err)
	}
	if !strings.Contains(script, "out.length>=1000") {
		t.Errorf("script did not clamp to maxSnapshotElements; got %q", firstLine(script))
	}
}

// TestBrowserStaleRefIsAnError is the property that justifies ref targeting
// over coordinates. A coordinate that outlives its layout clicks whatever is
// there now; a ref that outlives its page must say so.
func TestBrowserStaleRefIsAnError(t *testing.T) {
	for _, action := range []api.BrowserAction{api.BrowserClick, api.BrowserText} {
		t.Run(string(action), func(t *testing.T) {
			caller := newBrowserCaller(t, func(script string) *mcp.CallToolResult {
				if !strings.Contains(script, "document.contains(el)") {
					t.Error("ref actions must check the node is still in the document, not just present in the array")
				}
				return jsReply(t, map[string]any{"error": "stale_ref"})
			})

			out, _ := NewAdapter(caller).Browser(context.Background(),
				api.BrowserInput{Action: action, Ref: "e4"})
			if out.Code != api.CodeInvalidArgument {
				t.Fatalf("code = %q, want %q", out.Code, api.CodeInvalidArgument)
			}
			if !strings.Contains(out.Message, `"e4"`) || !strings.Contains(out.Message, "snapshot") {
				t.Errorf("message = %q, want it to name the ref and say to take a new snapshot", out.Message)
			}
		})
	}
}

// TestBrowserRejectsMalformedRefs keeps a bad ref from being silently turned
// into some other element's index.
func TestBrowserRejectsMalformedRefs(t *testing.T) {
	for _, ref := range []string{"4", "e", "ex", "e0", "e-1", ""} {
		in := api.BrowserInput{Action: api.BrowserClick, Ref: ref}
		out, _ := NewAdapter(&fakeCaller{}).Browser(context.Background(), in)
		if out.Code != api.CodeInvalidArgument {
			t.Errorf("ref %q: code = %q, want %q", ref, out.Code, api.CodeInvalidArgument)
		}
	}
}

// TestBrowserRefIndexIsOneBased pins the mapping between the public ref and the
// array slot, which is off-by-one bait.
func TestBrowserRefIndexIsOneBased(t *testing.T) {
	var script string
	caller := newBrowserCaller(t, func(s string) *mcp.CallToolResult {
		script = s
		return jsReply(t, map[string]any{"url": "u", "title": "t"})
	})

	if _, err := NewAdapter(caller).Browser(context.Background(),
		api.BrowserInput{Action: api.BrowserText, Ref: "e3"}); err != nil {
		t.Fatalf("Browser: %v", err)
	}
	if !strings.Contains(script, "refs[2]") {
		t.Errorf("ref e3 must resolve to refs[2]; script was %q", firstLine(script))
	}
}

// TestBrowserEscapesCallerText proves caller-supplied strings reach the page as
// JavaScript literals. Concatenating them raw would break on a quote and would
// let a caller inject script into the program being built.
func TestBrowserEscapesCallerText(t *testing.T) {
	var script string
	caller := newBrowserCaller(t, func(s string) *mcp.CallToolResult {
		script = s
		return jsReply(t, map[string]any{"url": "u", "title": "t", "value": "x"})
	})

	// Both quote styles plus a backslash: jsString emits a double-quoted
	// literal, so a bare ' is inert inside it but a " or \ must be escaped.
	nasty := `\"); alert("pwned"); //`
	if _, err := NewAdapter(caller).Browser(context.Background(),
		api.BrowserInput{Action: api.BrowserType, Ref: "e1", Text: nasty}); err != nil {
		t.Fatalf("Browser: %v", err)
	}
	// The text must appear only as the literal jsString produced; anything else
	// means it was concatenated raw and could close the string and run.
	if literal := jsString(nasty); !strings.Contains(script, literal) {
		t.Fatalf("caller text did not reach the script as a JS literal\nwant substring: %s\ngot script: %s", literal, script)
	}
	if strings.Contains(script, `; alert("pwned"); //`) {
		t.Fatalf("caller text escaped its literal and became code; script was %s", script)
	}
}

// TestJSStringEscapes pins the escaping itself, independent of any action.
func TestJSStringEscapes(t *testing.T) {
	cases := map[string]string{
		`plain`:     `"plain"`,
		`say "hi"`:  `"say \"hi\""`,
		"tab\there": `"tab\there"`,
		"new\nline": `"new\nline"`,
		`it's fine`: `"it's fine"`,
	}
	for in, want := range cases {
		if got := jsString(in); got != want {
			t.Errorf("jsString(%q) = %s, want %s", in, got, want)
		}
	}
	// A backslash must survive as an escaped backslash, not vanish.
	if got := jsString(`back\slash`); got != `"back\\slash"` {
		t.Errorf(`jsString("back\slash") = %s, want "back\\slash"`, got)
	}
}

// TestBrowserNavigateReadsBackTheSettledPage: assigning location.href only
// starts the navigation, so the state the script could see is the old page's.
func TestBrowserNavigateReadsBackTheSettledPage(t *testing.T) {
	var scripts []string
	caller := newBrowserCaller(t, func(s string) *mcp.CallToolResult {
		scripts = append(scripts, s)
		if len(scripts) == 1 {
			return jsReply(t, map[string]any{"url": "about:blank", "title": ""})
		}
		return jsReply(t, map[string]any{"url": "https://kairos.io/docs/", "title": "Documentation | Kairos"})
	})

	out, _ := NewAdapter(caller).Browser(context.Background(),
		api.BrowserInput{Action: api.BrowserNavigate, URL: "https://kairos.io/docs/"})
	if out.Code != "" {
		t.Fatalf("code = %q, want success: %s", out.Code, out.Message)
	}
	if len(scripts) != 2 {
		t.Fatalf("ran %d scripts, want 2 (navigate, then read back after settling)", len(scripts))
	}
	if out.Title != "Documentation | Kairos" || out.URL != "https://kairos.io/docs/" {
		t.Fatalf("out = %+v, want the settled page, not the pre-navigation one", out)
	}
}

// TestBrowserScrollReportsOffset covers the one action whose result is a
// number a caller may legitimately compare against zero, hence the pointer.
func TestBrowserScrollReportsOffset(t *testing.T) {
	caller := newBrowserCaller(t, func(string) *mcp.CallToolResult {
		return jsReply(t, map[string]any{"url": "u", "title": "t", "scroll_x": 0, "scroll_y": 400})
	})

	out, _ := NewAdapter(caller).Browser(context.Background(),
		api.BrowserInput{Action: api.BrowserScroll, Direction: api.DirectionDown})
	if out.ScrollY == nil || *out.ScrollY != 400 {
		t.Fatalf("scroll_y = %v, want 400", out.ScrollY)
	}
	if out.ScrollX == nil || *out.ScrollX != 0 {
		t.Fatalf("scroll_x = %v, want an explicit 0 rather than an omitted field", out.ScrollX)
	}
}

// TestUnwrapJSONPeelsTheDriversWrapping covers the reply shapes execute_javascript
// is known to produce, since how many times it wraps is a driver detail this
// contract should not depend on.
func TestUnwrapJSONPeelsTheDriversWrapping(t *testing.T) {
	want := "https://kairos.io/"
	cases := map[string]string{
		"bare object":  `{"url":"https://kairos.io/"}`,
		"json string":  `"{\"url\":\"https://kairos.io/\"}"`,
		"double wrap":  `"\"{\\\"url\\\":\\\"https://kairos.io/\\\"}\""`,
		"with padding": "  {\"url\":\"https://kairos.io/\"}\n",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var state pageState
			if err := unwrapJSON(payload, &state); err != nil {
				t.Fatalf("unwrapJSON: %v", err)
			}
			if state.URL != want {
				t.Fatalf("url = %q, want %q", state.URL, want)
			}
		})
	}

	var state pageState
	if err := unwrapJSON("", &state); err == nil {
		t.Error("empty reply must be an error, not a zero-valued success")
	}
	if err := unwrapJSON("not json at all", &state); err == nil {
		t.Error("unparseable reply must be an error")
	}
}

// TestBrowserErrorsDoNotClaimToBeComputerUse: a browser failure that reports
// itself as computer_use sends whoever reads the audit log to the wrong tool.
func TestBrowserErrorsDoNotClaimToBeComputerUse(t *testing.T) {
	caller := &fakeCaller{handler: func(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
		return nil, ErrDisconnected
	}}

	out, _ := NewAdapter(caller).Browser(context.Background(), api.BrowserInput{Action: api.BrowserSnapshot})
	if out.Code != api.CodeSessionUnavailable {
		t.Fatalf("code = %q, want %q", out.Code, api.CodeSessionUnavailable)
	}
	if strings.Contains(out.Message, "computer_use") {
		t.Fatalf("message = %q, want it attributed to browser", out.Message)
	}
	if !strings.HasPrefix(out.Message, "browser:") {
		t.Fatalf("message = %q, want a browser: prefix", out.Message)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
