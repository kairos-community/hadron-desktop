package smoke

// ui.go is the reference UI gate: it drives two real, third-party-shaped
// applications -- a GTK3 client and Chromium -- through nothing but the public
// MCP tools, and proves that what the appliance REPORTS matches what the
// applications actually DID.
//
// Each half uses the path that works on this appliance, which was settled by
// running it rather than by preference:
//
//   - GTK is driven by PIXELS. Nothing here is GTK-specific and nothing reads
//     the accessibility tree for state. A live appliance exposes five
//     accessibles for the entire fixture window, and the widgets carrying the
//     state are not among them; giving them accessible names and making them
//     focusable changed nothing, which is what proved the ceiling is in the
//     platform's AT-SPI exposure rather than the fixture's wiring.
//
//   - Chromium is driven by the BROWSER tool. Flatpak Chromium publishes no
//     AT-SPI DOM tree at all -- one node for the whole window -- which Phase 1
//     established and which is the reason the browser tool exists. Driving a
//     browser through the desktop accessibility path is not awkward here, it is
//     impossible.
//
// What keeps the gate honest in both halves is the same: every gesture is
// judged by state the appliance does not author -- the GTK app's own on-disk
// JSON, or the page's own rendered state block. A gesture that was never
// delivered cannot move either of them, so a pass means the input path really
// carried it. Asserting on a tool's own return value would prove nothing.
//
// GTK coordinates come from the fixture's own fixed layout (gtk_fixed_put with
// explicit positions), offset by the window's screen origin. That is
// deterministic without asking the toolkit anything.
//
// This file assumes the fixtures are already installed and RUNNING in the
// visible session; test/agent/run.sh uploads them, holds each one open with its
// own bash call, and afterwards cross-checks a public screenshot against a QMP
// capture.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// gtkStatePath is where the GTK fixture atomically persists its own counters.
const gtkStatePath = "/run/user/1000/hadron-cua-gtk-state.json"

// chromiumFixtureURL is the page the browser half drives.
const chromiumFixtureURL = "file:///home/agent/e2e/web/index.html"

// Application names list_applications reports. These are NOT window titles:
// Cua derives the name from WM_CLASS, so Chromium reports "Chromium" whatever
// the document is called, and the GTK fixture reports what GTK made of its
// prgname ("Hadron-cua-gtk" on a live run). Matched case-insensitively.
const (
	gtkAppName      = "hadron-cua-gtk"
	chromiumAppName = "chromium"
)

// uiTypedText is the literal string both type actions send.
const uiTypedText = "hadron"

// uiSettleMs is how long the desktop is given after a gesture, through the
// public wait action rather than a local sleep so a paused or wedged session
// surfaces instead of being papered over.
const uiSettleMs = 500

// The GTK fixture's layout, mirroring gtk_fixed_put in
// test/agent/fixtures/gtk3/main.c. Positions are relative to the window's
// origin. The numbers are duplicated from the fixture on purpose: a pixel gate
// needs coordinates that do not depend on the toolkit reporting anything, and a
// change on either side must be made on both.
type uiRect struct{ x, y, w, h int }

var (
	gtkClickButton  = uiRect{40, 35, 180, 55}
	gtkDoubleButton = uiRect{40, 120, 180, 70}
	gtkTextEntry    = uiRect{40, 220, 360, 45}
	gtkDragSource   = uiRect{40, 420, 150, 80}
	gtkDragTarget   = uiRect{520, 420, 150, 80}
	gtkScrollArea   = uiRect{700, 35, 170, 540}
)

func (r uiRect) center() (int, int) { return r.x + r.w/2, r.y + r.h/2 }

// fixtureState is the state both fixtures report, in the same shape. One type
// for both keeps the halves honest: a field only one of them reports would show
// up immediately as an unused assertion.
type fixtureState struct {
	Clicks       int    `json:"clicks"`
	DoubleClicks int    `json:"double_clicks"`
	Text         string `json:"text"`
	Key          string `json:"key"`
	Dragged      bool   `json:"dragged"`
	ScrollValue  int    `json:"scroll_value"`
}

// RunUI drives both reference fixtures through the public tool surface.
//
// Ordering within each half is load-bearing: both fixtures record only the LAST
// key seen, and typing is a run of key events, so the text check must precede
// the named-key check or the latter would assert against typing's leftovers.
func (s *Suite) RunUI(ctx context.Context) Report {
	r := Report{Mode: "ui"}

	sess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_user", "could not open the user MCP session"))
		return r
	}
	defer sess.Close()

	r.Checks = append(r.Checks, s.runGTK(ctx, sess)...)
	r.Checks = append(r.Checks, s.runChromium(ctx, sess)...)
	return r
}

// ---------------------------------------------------------------------------
// GTK: pixels in, application state out
// ---------------------------------------------------------------------------

var gtkDependents = []string{
	"gtk_click", "gtk_double_click", "gtk_drag", "gtk_scroll", "gtk_text_entry", "gtk_named_key",
}

func (s *Suite) runGTK(ctx context.Context, sess *mcp.ClientSession) []CheckResult {
	const present = "gtk_window_present"

	app, fail := s.locateApp(ctx, sess, gtkAppName)
	if fail != nil {
		return expandFailure(fail, present, gtkDependents...)
	}
	origin, fail := s.windowOrigin(ctx, sess, app.PID)
	if fail != nil {
		return expandFailure(fail, present, gtkDependents...)
	}
	if f := s.focus(ctx, sess, app.PID); f != nil {
		return expandFailure(f, present, gtkDependents...)
	}

	pid := app.PID
	checks := []CheckResult{pass(present,
		fmt.Sprintf("found the GTK fixture window (pid %d) with its origin at %d,%d", pid, origin.x, origin.y))}

	at := func(r uiRect) (int, int) {
		x, y := r.center()
		return origin.x + x, origin.y + y
	}

	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_click",
		func() *terminalFail {
			x, y := at(gtkClickButton)
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionClick, PID: &pid, X: &x, Y: &y, Button: api.ButtonLeft})
		},
		func(before, after fixtureState) error {
			if after.Clicks != before.Clicks+1 {
				return fmt.Errorf("clicks went %d -> %d, want +1", before.Clicks, after.Clicks)
			}
			return nil
		}))

	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_double_click",
		func() *terminalFail {
			x, y := at(gtkDoubleButton)
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionDoubleClick, PID: &pid, X: &x, Y: &y, Button: api.ButtonLeft})
		},
		func(before, after fixtureState) error {
			if after.DoubleClicks != before.DoubleClicks+1 {
				return fmt.Errorf("double clicks went %d -> %d, want +1", before.DoubleClicks, after.DoubleClicks)
			}
			return nil
		}))

	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_drag",
		func() *terminalFail {
			fx, fy := at(gtkDragSource)
			tx, ty := at(gtkDragTarget)
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionDrag, PID: &pid,
				FromX: &fx, FromY: &fy, ToX: &tx, ToY: &ty, Button: api.ButtonLeft})
		},
		func(before, after fixtureState) error {
			if before.Dragged {
				return fmt.Errorf("the fixture already recorded a drop before the gesture; the check would be vacuous")
			}
			if !after.Dragged {
				return fmt.Errorf("the fixture never recorded a drop")
			}
			return nil
		}))

	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_scroll",
		func() *terminalFail {
			x, y := at(gtkScrollArea)
			amount := 5
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionScroll, PID: &pid, X: &x, Y: &y,
				Direction: api.DirectionDown, Amount: &amount})
		},
		func(before, after fixtureState) error {
			if after.ScrollValue <= before.ScrollValue {
				return fmt.Errorf("scroll value went %d -> %d, want an increase",
					before.ScrollValue, after.ScrollValue)
			}
			return nil
		}))

	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_text_entry",
		func() *terminalFail {
			x, y := at(gtkTextEntry)
			if f := s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionClick, PID: &pid, X: &x, Y: &y, Button: api.ButtonLeft}); f != nil {
				return f
			}
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionType, PID: &pid, Text: uiTypedText})
		},
		func(before, after fixtureState) error {
			if !strings.Contains(after.Text, uiTypedText) {
				return fmt.Errorf("the entry holds %q, want it to contain %q", after.Text, uiTypedText)
			}
			return nil
		}))

	// Last: typing above overwrites the fixture's single key field.
	checks = append(checks, s.gtkGesture(ctx, sess, "gtk_named_key",
		func() *terminalFail {
			return s.act(ctx, sess, api.ComputerUseInput{
				Action: api.ActionKey, PID: &pid, Key: "Return"})
		},
		func(before, after fixtureState) error {
			if after.Key != "Return" {
				return fmt.Errorf("the fixture recorded key %q, want %q", after.Key, "Return")
			}
			return nil
		}))

	return checks
}

// gtkGesture performs one gesture and judges it by the fixture's OWN state
// file, read before and after through the public read_file tool.
//
// The application's state is the honest witness: it cannot agree with a gesture
// that was never delivered, and unlike a pixel diff it says WHAT changed rather
// than merely that something did.
func (s *Suite) gtkGesture(ctx context.Context, sess *mcp.ClientSession, name string,
	gesture func() *terminalFail, judge func(before, after fixtureState) error) CheckResult {

	before, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if f := gesture(); f != nil {
		return f.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if err := judge(before, after); err != nil {
		return failAssert(name, err.Error())
	}
	return pass(name, "the gesture landed and the application recorded it")
}

// gtkState reads the fixture's state file with the public read_file tool.
func (s *Suite) gtkState(ctx context.Context, sess *mcp.ClientSession) (fixtureState, *terminalFail) {
	var out api.ReadFileOutput
	if err := s.callInto(ctx, sess, api.ToolReadFile, api.ReadFileInput{Path: gtkStatePath}, &out); err != nil {
		return fixtureState{}, &terminalFail{transport: true, detail: "read_file call failed"}
	}
	if out.Code != "" {
		return fixtureState{}, &terminalFail{
			detail: fmt.Sprintf("read_file of the fixture state reported %s: %s", out.Code, out.Message)}
	}
	return decodeFixtureState(out.Content, "the fixture's state file")
}

// ---------------------------------------------------------------------------
// Chromium: the browser tool
// ---------------------------------------------------------------------------

var chromiumDependents = []string{
	"chromium_navigate", "chromium_click", "chromium_type", "chromium_key", "chromium_scroll",
}

func (s *Suite) runChromium(ctx context.Context, sess *mcp.ClientSession) []CheckResult {
	const present = "chromium_window_present"

	app, fail := s.locateApp(ctx, sess, chromiumAppName)
	if fail != nil {
		return expandFailure(fail, present, chromiumDependents...)
	}
	checks := []CheckResult{pass(present, fmt.Sprintf("found the Chromium fixture window (pid %d)", app.PID))}

	// Put the browser on the fixture page explicitly. It may already be there,
	// but asserting it costs one call and removes a whole class of "why is the
	// page empty" confusion later.
	nav, f := s.browser(ctx, sess, api.BrowserInput{Action: api.BrowserNavigate, URL: chromiumFixtureURL})
	if f != nil {
		return append(checks, expandFailure(f, "chromium_navigate", chromiumDependents[1:]...)...)
	}
	if !strings.Contains(nav.URL, "index.html") {
		return append(checks, expandFailure(
			&terminalFail{detail: fmt.Sprintf("landed on %q, want the fixture page", nav.URL)},
			"chromium_navigate", chromiumDependents[1:]...)...)
	}
	checks = append(checks, pass("chromium_navigate", "the browser tool loaded the fixture page: "+nav.Title))

	checks = append(checks, s.browserGesture(ctx, sess, "chromium_click", "click",
		func(ref string) api.BrowserInput { return api.BrowserInput{Action: api.BrowserClick, Ref: ref} },
		func(before, after fixtureState) error {
			if after.Clicks != before.Clicks+1 {
				return fmt.Errorf("clicks went %d -> %d, want +1", before.Clicks, after.Clicks)
			}
			return nil
		}))

	checks = append(checks, s.browserGesture(ctx, sess, "chromium_type", "text",
		func(ref string) api.BrowserInput {
			return api.BrowserInput{Action: api.BrowserType, Ref: ref, Text: uiTypedText}
		},
		func(before, after fixtureState) error {
			if !strings.Contains(after.Text, uiTypedText) {
				return fmt.Errorf("the input holds %q, want it to contain %q", after.Text, uiTypedText)
			}
			return nil
		}))

	// Last, for the same reason as the GTK half.
	checks = append(checks, s.browserGesture(ctx, sess, "chromium_key", "",
		func(string) api.BrowserInput { return api.BrowserInput{Action: api.BrowserPress, Key: "Enter"} },
		func(before, after fixtureState) error {
			if after.Key != "Enter" {
				return fmt.Errorf("the page recorded key %q, want %q", after.Key, "Enter")
			}
			return nil
		}))

	checks = append(checks, s.browserGesture(ctx, sess, "chromium_scroll", "",
		func(string) api.BrowserInput {
			amount := 400
			return api.BrowserInput{Action: api.BrowserScroll, Direction: api.DirectionDown, Amount: &amount}
		},
		func(before, after fixtureState) error {
			if after.ScrollValue <= before.ScrollValue {
				return fmt.Errorf("scroll value went %d -> %d, want an increase",
					before.ScrollValue, after.ScrollValue)
			}
			return nil
		}))

	return checks
}

// browserGesture performs one browser action and judges it by the page's own
// rendered state block.
//
// When match is non-empty the action needs a ref, so the element is located in
// a FRESH snapshot first -- which also proves the page still exposes it, and
// means a ref can never outlive the page it came from.
func (s *Suite) browserGesture(ctx context.Context, sess *mcp.ClientSession, name, match string,
	build func(ref string) api.BrowserInput, judge func(before, after fixtureState) error) CheckResult {

	before, fail := s.chromiumState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}

	ref := ""
	if match != "" {
		snap, f := s.browser(ctx, sess, api.BrowserInput{Action: api.BrowserSnapshot})
		if f != nil {
			return f.named(name)
		}
		var seen []string
		for _, el := range snap.Elements {
			seen = append(seen, el.Name)
			if strings.Contains(strings.ToLower(el.Name), strings.ToLower(match)) {
				ref = el.Ref
				break
			}
		}
		if ref == "" {
			return failAssert(name, fmt.Sprintf("no element matching %q among %d snapshot elements: %v",
				match, len(snap.Elements), seen))
		}
	}

	if _, f := s.browser(ctx, sess, build(ref)); f != nil {
		return f.named(name)
	}

	after, fail := s.chromiumState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if err := judge(before, after); err != nil {
		return failAssert(name, err.Error())
	}
	return pass(name, "the browser action landed and the page recorded it")
}

// chromiumState reads the page's own rendered state block through the browser
// tool's text action -- the same text a human reads off the screen.
func (s *Suite) chromiumState(ctx context.Context, sess *mcp.ClientSession) (fixtureState, *terminalFail) {
	out, fail := s.browser(ctx, sess, api.BrowserInput{Action: api.BrowserText})
	if fail != nil {
		return fixtureState{}, fail
	}
	return decodeFixtureState(out.Text, "the page's state block")
}

func (s *Suite) browser(ctx context.Context, sess *mcp.ClientSession, in api.BrowserInput) (api.BrowserOutput, *terminalFail) {
	var out api.BrowserOutput
	if err := s.callInto(ctx, sess, api.ToolBrowser, in, &out); err != nil {
		return out, &terminalFail{transport: true, detail: fmt.Sprintf("browser %s call failed", in.Action)}
	}
	if out.Code != "" {
		return out, &terminalFail{detail: fmt.Sprintf("browser %s reported %s: %s", in.Action, out.Code, out.Message)}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// decodeFixtureState pulls the JSON object out of text that CONTAINS it. Both
// fixtures embed their state in surrounding content, so the object is located
// rather than assumed to be the whole string.
func decodeFixtureState(text, what string) (fixtureState, *terminalFail) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return fixtureState{}, &terminalFail{detail: what + " contained no state object"}
	}
	var state fixtureState
	if err := json.Unmarshal([]byte(text[start:end+1]), &state); err != nil {
		return fixtureState{}, &terminalFail{detail: what + " is not valid JSON"}
	}
	return state, nil
}

type uiPoint struct{ x, y int }

// windowOrigin returns an application window's screen position, so the
// fixture's own layout coordinates become screen coordinates.
//
// get_window_state reports frames in SCREEN space, and the outermost of them is
// the window itself. This is the one thing the gate asks the accessibility
// layer for, and it asks only for geometry -- not for state, and not for any
// individual widget -- which is exactly the part that works on this appliance.
func (s *Suite) windowOrigin(ctx context.Context, sess *mcp.ClientSession, pid int) (uiPoint, *terminalFail) {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionAccessibility, Scope: api.ScopeWindow, PID: &pid,
	}, &out); err != nil {
		return uiPoint{}, &terminalFail{transport: true, detail: "could not read the window geometry"}
	}
	if out.Code != "" {
		return uiPoint{}, &terminalFail{
			detail: fmt.Sprintf("window geometry reported %s: %s", out.Code, out.Message)}
	}
	best, area := uiPoint{}, -1
	for _, el := range out.Elements {
		if el.Width*el.Height > area {
			area = el.Width * el.Height
			best = uiPoint{el.X, el.Y}
		}
	}
	if area <= 0 {
		return uiPoint{}, &terminalFail{detail: "the window reported no usable geometry"}
	}
	return best, nil
}

// locateApp finds a running application whose reported name contains name,
// case-insensitively. Cua derives that name from WM_CLASS, which is why this
// matches an application name and never a window title.
func (s *Suite) locateApp(ctx context.Context, sess *mcp.ClientSession, name string) (api.ApplicationInfo, *terminalFail) {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionListApplications,
	}, &out); err != nil {
		return api.ApplicationInfo{}, &terminalFail{transport: true, detail: "list_applications call failed"}
	}
	if out.Code != "" {
		return api.ApplicationInfo{}, &terminalFail{
			detail: fmt.Sprintf("list_applications reported %s: %s", out.Code, out.Message)}
	}
	var seen []string
	for _, app := range out.Applications {
		seen = append(seen, app.Name)
		if strings.Contains(strings.ToLower(app.Name), strings.ToLower(name)) {
			return app, nil
		}
	}
	return api.ApplicationInfo{}, &terminalFail{
		detail: fmt.Sprintf("no application named %q among %d listed: %v; the fixture is not running",
			name, len(out.Applications), seen)}
}

func (s *Suite) focus(ctx context.Context, sess *mcp.ClientSession, pid int) *terminalFail {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionFocusApplication, PID: &pid,
	}, &out); err != nil {
		return &terminalFail{transport: true, detail: "focus_application call failed"}
	}
	if out.Code != "" {
		return &terminalFail{detail: fmt.Sprintf("focus_application reported %s: %s", out.Code, out.Message)}
	}
	return nil
}

// act dispatches one computer_use gesture and lets the desktop settle through
// the public wait action.
func (s *Suite) act(ctx context.Context, sess *mcp.ClientSession, in api.ComputerUseInput) *terminalFail {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, in, &out); err != nil {
		return &terminalFail{transport: true, detail: fmt.Sprintf("computer_use %s call failed", in.Action)}
	}
	if out.Code != "" {
		return &terminalFail{detail: fmt.Sprintf("computer_use %s reported %s: %s", in.Action, out.Code, out.Message)}
	}
	settle := uiSettleMs
	var wait api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionWait, DurationMs: &settle,
	}, &wait); err != nil {
		return &terminalFail{transport: true, detail: "computer_use wait call failed"}
	}
	return nil
}

// expandFailure turns one setup failure into the setup check plus an explicit
// failure for every check that could not run. A skipped check that simply
// vanishes from the report reads as a smaller run; a named failure reads as
// what it is.
func expandFailure(fail *terminalFail, setupName string, dependents ...string) []CheckResult {
	results := []CheckResult{fail.named(setupName)}
	for _, name := range dependents {
		results = append(results, CheckResult{
			Name:      name,
			Passed:    false,
			Transport: fail.transport,
			Detail:    "not evaluated: " + setupName + " failed",
		})
	}
	return results
}
