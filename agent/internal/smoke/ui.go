package smoke

// ui.go is the reference UI gate: it drives two real, third-party-shaped
// applications -- a GTK3 client and Chromium -- through nothing but the eight
// public MCP tools, and proves that what the appliance REPORTS about the
// desktop matches what the applications actually DID.
//
// The gate exists because every layer between the model and the pixels can
// regress independently and silently: the Cua driver, the XLibre server, the
// window manager, the ATK/AT-SPI bridge, and Chromium's own X11 (ozone) path.
// A capture that returns bytes proves none of that -- checkComputerUseCapture
// already covers plumbing. What this file adds is a CLOSED LOOP: every gesture
// is aimed using coordinates taken from the accessibility tree, and the effect
// is then read back from a source the appliance does not control (the GTK app's
// own on-disk JSON, or the page's own rendered state text). If accessibility
// geometry drifts from the real screen -- a scaling bug, a wrong window origin,
// a stale frame -- the gesture lands somewhere harmless and the application
// state never moves. That is the regression class this gate is here to catch,
// and it cannot be caught by asserting on the tool's own return value.
//
// Deliberately NOT used here: the `browser` tool. It is the eighth public tool
// and it works, but it is CDP-backed -- it talks to Chromium's own debug
// protocol. Asserting Chromium behaviour through CDP would be asking the
// browser to grade its own homework: a page can report "clicks: 1" over CDP
// while nothing was ever painted or delivered through X11. Every Chromium
// assertion below therefore goes through computer_use only, so the input path
// (XLibre -> ozone) and the output path (accessibility + pixels) are both
// genuinely under test. checkBrowserReachable in checks.go covers the browser
// tool's own plumbing.
//
// This file assumes the fixtures are already installed and RUNNING in the
// visible session; the shell harness (test/agent/run.sh) uploads, launches, and
// afterwards cross-checks a public screenshot against a QMP capture. A missing
// window is reported as a plain assertion failure with the window title in the
// detail, never a panic -- an operator reading the artifact must be able to
// tell "the fixture never started" apart from "the fixture ignored the input".

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// gtkStatePath is where the GTK fixture atomically persists its own counters.
// It is application-owned: the appliance never writes it, so reading it back
// through the public read_file tool is independent evidence that the gesture
// reached the application, not merely that computer_use returned success.
const gtkStatePath = "/run/user/1000/hadron-cua-gtk-state.json"

// Application names list_applications reports for the two fixtures.
//
// These are NOT the window titles. list_applications groups windows by the
// application name Cua derives from WM_CLASS, so matching a page or window
// title against it never succeeds: Chromium reports "Chromium" whatever the
// document is called, and the GTK fixture reports the program name GTK turns
// into its WM_CLASS. Verified on a live appliance, where the fixture appears as
// {"name": "Hadron-cua-gtk"} -- GTK capitalises the first letter of prgname.
//
// The match is a case-insensitive substring so the capitalisation GTK applies,
// and any suffix Chromium appends, cannot break it.
const (
	gtkWindowTitle      = "hadron-cua-gtk"
	chromiumWindowTitle = "chromium"
)

// Accessible names both fixtures pin on their controls. They are identical
// across the GTK and HTML fixtures on purpose: the same suite logic can aim at
// either toolkit, which is what makes a divergence between them meaningful.
const (
	nameClickCount       = "Click count"
	nameDoubleClickCount = "Double click count"
	nameTextInput        = "Text input"
	nameDragSource       = "Drag source"
	nameDragTarget       = "Drag target"
	nameScrollableRows   = "Scrollable rows"
)

// uiTypedText is the literal string the type action sends. It is deliberately
// lowercase ASCII with no modifiers: this gate tests that typing arrives at
// all, not that the keymap handles shifted or composed characters, and a
// failure here must be unambiguous about which of the two it was.
const uiTypedText = "hadron"

// uiNamedKey is the named key pressed by the key checks. Return is chosen
// because the two toolkits report it under DIFFERENT names -- GDK's
// gdk_keyval_name yields "Return", the DOM's KeyboardEvent.key yields "Enter"
// -- so a suite that expected one name everywhere would silently prove nothing
// on one of the two platforms.
const (
	uiNamedKey       = "Return"
	gtkKeyName       = "Return"
	chromiumKeyName  = "Enter"
	uiScrollNotches  = 5
	uiSettleMs       = 400
	uiMaxElements    = 2000
	uiMaxTreeDepth   = 32
	uiCaptureMinimum = 64
)

// fixtureState is the state document BOTH fixtures expose, byte-identical in
// shape. The GTK app writes it to gtkStatePath; the web page renders it into
// its own #state element, from which it is read back out of the accessibility
// tree. One Go type for both keeps the two halves of the gate honest: a field
// only one fixture reports would show up immediately as an unused assertion.
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
// Check order within each fixture is not arbitrary. Both fixtures record the
// LAST key seen in a single `key` field, and typing text is a sequence of key
// presses -- so the text check must run before the named-key check or the text
// check would clobber the very field the key check asserts on. Likewise the
// click check runs before the double-click check so the two counters can be
// asserted against independently observed baselines.
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
// GTK fixture
// ---------------------------------------------------------------------------

// runGTK locates the GTK window and runs the six gesture checks against it.
// If the window is absent every dependent check is reported as a distinct,
// named failure rather than being silently dropped: a report that is missing
// checks looks like a shorter run, while a report full of "window not found"
// failures says exactly what went wrong.
func (s *Suite) runGTK(ctx context.Context, sess *mcp.ClientSession) []CheckResult {
	app, fail := s.locateApp(ctx, sess, gtkWindowTitle)
	if fail != nil {
		return expandFailure(fail, "gtk_window_present",
			"gtk_click", "gtk_double_click", "gtk_drag", "gtk_scroll", "gtk_text_entry", "gtk_named_key")
	}

	checks := []CheckResult{pass("gtk_window_present",
		fmt.Sprintf("found the GTK fixture window (pid %d)", app.PID))}

	// Focus once up front. Every gesture below aims at absolute screen
	// coordinates taken from the accessibility tree, so an unfocused or
	// occluded window would send input to whatever is on top instead -- and the
	// JSON assertions would then fail for a reason that has nothing to do with
	// the code under test.
	if fail := s.focus(ctx, sess, app.PID); fail != nil {
		return append(checks, expandFailure(fail, "gtk_focus",
			"gtk_click", "gtk_double_click", "gtk_drag", "gtk_scroll", "gtk_text_entry", "gtk_named_key")...)
	}

	return append(checks,
		s.checkGTKClick(ctx, sess, app.PID),
		s.checkGTKDoubleClick(ctx, sess, app.PID),
		s.checkGTKDrag(ctx, sess, app.PID),
		s.checkGTKScroll(ctx, sess, app.PID),
		s.checkGTKTextEntry(ctx, sess, app.PID),
		s.checkGTKNamedKey(ctx, sess, app.PID),
	)
}

// checkGTKClick presses the click button once. The button is located by its
// accessible name and struck at the CENTRE OF ITS REPORTED RECTANGLE, so a
// pass means the accessibility geometry and the real pointer coordinate space
// agree; the counter in the application's own JSON is what proves the press
// was delivered rather than merely dispatched.
func (s *Suite) checkGTKClick(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_click"

	before, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	el, fail := s.locateElement(ctx, sess, pid, nameClickCount)
	if fail != nil {
		return fail.named(name)
	}
	x, y := center(el)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionClick, X: &x, Y: &y, Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}

	want := before.Clicks + 1
	if fail := s.expectAccessibleText(ctx, sess, pid, fmt.Sprintf("Clicks: %d", want)); fail != nil {
		return fail.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if after.Clicks != want {
		return failAssert(name, fmt.Sprintf("clicks went %d -> %d, want %d", before.Clicks, after.Clicks, want))
	}
	return pass(name, fmt.Sprintf("click at the accessible centre raised clicks to %d, confirmed in the app's JSON", after.Clicks))
}

// checkGTKDoubleClick proves the double-click is delivered as ONE
// GDK_2BUTTON_PRESS and not as two unrelated presses. The fixture counts only
// the compound event, so a driver that lost the timing (sending two clicks too
// far apart) leaves double_clicks untouched even though pixels changed.
func (s *Suite) checkGTKDoubleClick(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_double_click"

	before, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	el, fail := s.locateElement(ctx, sess, pid, nameDoubleClickCount)
	if fail != nil {
		return fail.named(name)
	}
	x, y := center(el)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionDoubleClick, X: &x, Y: &y, Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}

	want := before.DoubleClicks + 1
	if fail := s.expectAccessibleText(ctx, sess, pid, fmt.Sprintf("Double clicks: %d", want)); fail != nil {
		return fail.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if after.DoubleClicks != want {
		return failAssert(name, fmt.Sprintf("double_clicks went %d -> %d, want %d", before.DoubleClicks, after.DoubleClicks, want))
	}
	return pass(name, fmt.Sprintf("double click raised double_clicks to %d, confirmed in the app's JSON", after.DoubleClicks))
}

// checkGTKDrag drags the source onto the target. This is the strongest single
// gesture in the suite: GTK's drag-and-drop only completes if press, a run of
// intermediate motion events, and release all arrive in order with the X11
// selection handshake in between. A driver that "drags" by teleporting the
// pointer and releasing produces a visually plausible screenshot and a
// `dragged: false` state.
func (s *Suite) checkGTKDrag(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_drag"

	before, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	// A fixture that already reports a completed drop makes the assertion
	// below vacuous, so refuse to run rather than record a pass that proves
	// nothing about this gesture.
	if before.Dragged {
		return failAssert(name, "the fixture already reported dragged=true before the gesture; the assertion would be vacuous")
	}
	source, fail := s.locateElement(ctx, sess, pid, nameDragSource)
	if fail != nil {
		return fail.named(name)
	}
	target, fail := s.locateElement(ctx, sess, pid, nameDragTarget)
	if fail != nil {
		return fail.named(name)
	}
	fromX, fromY := center(source)
	toX, toY := center(target)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionDrag,
		FromX:  &fromX, FromY: &fromY, ToX: &toX, ToY: &toY,
		Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}

	if fail := s.expectAccessibleText(ctx, sess, pid, "Drag target: dropped"); fail != nil {
		return fail.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if !after.Dragged {
		return failAssert(name, "the drop was not accepted: the app still reports dragged=false")
	}
	return pass(name, "drag from the accessible source to the accessible target was accepted by the app")
}

// checkGTKScroll scrolls the row list and asserts the movement two ways that
// fail independently. The JSON scroll_value proves the adjustment moved; the
// accessibility geometry of a known row proves the CONTENT moved with it. They
// can disagree: a toolkit can update its adjustment while the AT-SPI bridge
// keeps handing out stale, pre-scroll rectangles, which is precisely the bug
// that makes a model click the wrong row forever after.
func (s *Suite) checkGTKScroll(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_scroll"

	before, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	rows, fail := s.locateElement(ctx, sess, pid, nameScrollableRows)
	if fail != nil {
		return fail.named(name)
	}
	// The first row is the geometry witness: after scrolling down it must have
	// moved UP the screen. Captured before the gesture so the comparison is
	// against an observed value, not a hard-coded coordinate.
	firstRowBefore, fail := s.locateElement(ctx, sess, pid, "Scrollable row 01")
	if fail != nil {
		return fail.named(name)
	}

	x, y := center(rows)
	amount := uiScrollNotches
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionScroll, X: &x, Y: &y,
		Direction: api.DirectionDown, Amount: &amount,
	}); fail != nil {
		return fail.named(name)
	}

	firstRowAfter, fail := s.locateElement(ctx, sess, pid, "Scrollable row 01")
	if fail != nil {
		return fail.named(name)
	}
	if firstRowAfter.Y >= firstRowBefore.Y {
		return failAssert(name, fmt.Sprintf(
			"accessibility still reports the first row at y=%d (was y=%d) after scrolling down; geometry did not follow the scroll",
			firstRowAfter.Y, firstRowBefore.Y))
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if after.ScrollValue <= before.ScrollValue {
		return failAssert(name, fmt.Sprintf("scroll_value went %d -> %d, want an increase", before.ScrollValue, after.ScrollValue))
	}
	return pass(name, fmt.Sprintf(
		"scroll moved the accessible first row from y=%d to y=%d and raised scroll_value to %d",
		firstRowBefore.Y, firstRowAfter.Y, after.ScrollValue))
}

// checkGTKTextEntry focuses the entry by clicking its accessible rectangle and
// types a literal string. Focus-by-click is deliberate: it is the only path a
// model actually has, and it exercises the window manager's focus handling as
// well as the keyboard route. The entry's CONTENT is asserted from the app's
// JSON because ATK exposes an entry's text as an AtkText value, not as the
// accessible name -- see the note on accessibility limits at the bottom of this
// file.
func (s *Suite) checkGTKTextEntry(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_text_entry"

	el, fail := s.locateElement(ctx, sess, pid, nameTextInput)
	if fail != nil {
		return fail.named(name)
	}
	x, y := center(el)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionClick, X: &x, Y: &y, Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}
	if fail := s.act(ctx, sess, api.ComputerUseInput{Action: api.ActionType, Text: uiTypedText}); fail != nil {
		return fail.named(name)
	}

	// The entry must still be resolvable in a FRESH tree afterwards. A toolkit
	// that rebuilt or lost its accessible object on focus is a real failure
	// mode, and it would leave every later element_index dangling.
	if fail := s.expectAccessibleText(ctx, sess, pid, nameTextInput); fail != nil {
		return fail.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if after.Text != uiTypedText {
		return failAssert(name, fmt.Sprintf("entry text is %q, want %q", after.Text, uiTypedText))
	}
	return pass(name, fmt.Sprintf("typed %d characters into the accessible entry, confirmed in the app's JSON", len(uiTypedText)))
}

// checkGTKNamedKey presses a NAMED key rather than a character. Named keys take
// a different route than typed text -- they must be mapped to a keysym and sent
// as a synthetic press/release pair -- so a keymap regression can break Return
// while ordinary typing still works. The fixture records GDK's own name for
// what it received, which is what makes this an assertion about the key that
// ARRIVED rather than the key that was requested.
func (s *Suite) checkGTKNamedKey(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "gtk_named_key"

	if fail := s.act(ctx, sess, api.ComputerUseInput{Action: api.ActionKey, Key: uiNamedKey}); fail != nil {
		return fail.named(name)
	}
	if fail := s.expectAccessibleText(ctx, sess, pid, "Named key: "+gtkKeyName); fail != nil {
		return fail.named(name)
	}
	after, fail := s.gtkState(ctx, sess)
	if fail != nil {
		return fail.named(name)
	}
	if after.Key != gtkKeyName {
		return failAssert(name, fmt.Sprintf("app received key %q, want %q", after.Key, gtkKeyName))
	}
	return pass(name, fmt.Sprintf("named key %s arrived as GDK %q, confirmed in the app's JSON", uiNamedKey, after.Key))
}

// ---------------------------------------------------------------------------
// Chromium fixture
// ---------------------------------------------------------------------------

// runChromium locates the Chromium window and runs the five gesture checks.
//
// Chromium has no application-owned file to read back, so every assertion here
// rests on two INDEPENDENT observations of the same window: a freshly queried
// accessibility tree (which carries the page's rendered state text) and a fresh
// pixel capture that must differ from the pre-gesture one. Requiring both is
// what makes the check meaningful without CDP -- accessibility alone can report
// a DOM mutation that was never composited, and pixels alone cannot say what
// changed.
func (s *Suite) runChromium(ctx context.Context, sess *mcp.ClientSession) []CheckResult {
	app, fail := s.locateApp(ctx, sess, chromiumWindowTitle)
	if fail != nil {
		return expandFailure(fail, "chromium_window_present",
			"chromium_click", "chromium_drag", "chromium_scroll", "chromium_type", "chromium_key")
	}

	checks := []CheckResult{pass("chromium_window_present",
		fmt.Sprintf("found the Chromium fixture window (pid %d)", app.PID))}

	if fail := s.focus(ctx, sess, app.PID); fail != nil {
		return append(checks, expandFailure(fail, "chromium_focus",
			"chromium_click", "chromium_drag", "chromium_scroll", "chromium_type", "chromium_key")...)
	}

	// Same ordering constraint as GTK: the page's keydown listener is on
	// document, so typing overwrites `key`. Text before key.
	return append(checks,
		s.checkChromiumClick(ctx, sess, app.PID),
		s.checkChromiumDrag(ctx, sess, app.PID),
		s.checkChromiumScroll(ctx, sess, app.PID),
		s.checkChromiumType(ctx, sess, app.PID),
		s.checkChromiumKey(ctx, sess, app.PID),
	)
}

func (s *Suite) checkChromiumClick(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "chromium_click"
	return s.chromiumGesture(ctx, sess, pid, name, nameClickCount,
		func(el *api.AccessibilityElement) api.ComputerUseInput {
			x, y := center(el)
			return api.ComputerUseInput{Action: api.ActionClick, X: &x, Y: &y, Button: api.ButtonLeft}
		},
		func(before, after fixtureState) string {
			if after.Clicks != before.Clicks+1 {
				return fmt.Sprintf("clicks went %d -> %d, want %d", before.Clicks, after.Clicks, before.Clicks+1)
			}
			return ""
		})
}

// checkChromiumDrag exercises HTML5 drag-and-drop, which Chromium implements on
// top of the platform's own drag protocol. It is the assertion most sensitive
// to the ozone/X11 path being wired correctly, and it is exactly the one a
// CDP-driven test would fake away: dispatching synthetic drag events over the
// debug protocol proves nothing about whether real pointer motion reaches the
// renderer.
func (s *Suite) checkChromiumDrag(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "chromium_drag"

	before, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if before.Dragged {
		return failAssert(name, "the page already reported dragged=true before the gesture; the assertion would be vacuous")
	}
	source, fail := s.locateElement(ctx, sess, pid, nameDragSource)
	if fail != nil {
		return fail.named(name)
	}
	target, fail := s.locateElement(ctx, sess, pid, nameDragTarget)
	if fail != nil {
		return fail.named(name)
	}
	shotBefore, fail := s.capture(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}

	fromX, fromY := center(source)
	toX, toY := center(target)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionDrag,
		FromX:  &fromX, FromY: &fromY, ToX: &toX, ToY: &toY,
		Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}

	after, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if !after.Dragged {
		return failAssert(name, "the page still reports dragged=false after the drag gesture")
	}
	if fail := s.expectRepaint(ctx, sess, pid, shotBefore); fail != nil {
		return fail.named(name)
	}
	return pass(name, "HTML5 drag completed through real pointer motion and the window repainted")
}

func (s *Suite) checkChromiumScroll(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "chromium_scroll"
	return s.chromiumGesture(ctx, sess, pid, name, nameScrollableRows,
		func(el *api.AccessibilityElement) api.ComputerUseInput {
			x, y := center(el)
			amount := uiScrollNotches
			return api.ComputerUseInput{
				Action: api.ActionScroll, X: &x, Y: &y,
				Direction: api.DirectionDown, Amount: &amount,
			}
		},
		func(before, after fixtureState) string {
			if after.ScrollValue <= before.ScrollValue {
				return fmt.Sprintf("scroll_value went %d -> %d, want an increase", before.ScrollValue, after.ScrollValue)
			}
			return ""
		})
}

// checkChromiumType clicks the input to focus it and types. Unlike the GTK
// entry, the page mirrors its input value into its own rendered state text, so
// the typed string IS readable from the accessibility tree -- no side channel
// required.
func (s *Suite) checkChromiumType(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "chromium_type"

	el, fail := s.locateElement(ctx, sess, pid, nameTextInput)
	if fail != nil {
		return fail.named(name)
	}
	shotBefore, fail := s.capture(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}

	x, y := center(el)
	if fail := s.act(ctx, sess, api.ComputerUseInput{
		Action: api.ActionClick, X: &x, Y: &y, Button: api.ButtonLeft,
	}); fail != nil {
		return fail.named(name)
	}
	if fail := s.act(ctx, sess, api.ComputerUseInput{Action: api.ActionType, Text: uiTypedText}); fail != nil {
		return fail.named(name)
	}

	after, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if after.Text != uiTypedText {
		return failAssert(name, fmt.Sprintf("page reports text %q, want %q", after.Text, uiTypedText))
	}
	if fail := s.expectRepaint(ctx, sess, pid, shotBefore); fail != nil {
		return fail.named(name)
	}
	return pass(name, fmt.Sprintf("typed text reached the page (%d characters) and the window repainted", len(uiTypedText)))
}

// checkChromiumKey asserts the DOM's name for the key, not X11's. The same
// physical Return that GDK reports as "Return" surfaces in the DOM as "Enter";
// asserting the platform-correct name on each side is what proves the key was
// translated by the real input stack rather than echoed back by the driver.
func (s *Suite) checkChromiumKey(ctx context.Context, sess *mcp.ClientSession, pid int) CheckResult {
	const name = "chromium_key"

	shotBefore, fail := s.capture(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if fail := s.act(ctx, sess, api.ComputerUseInput{Action: api.ActionKey, Key: uiNamedKey}); fail != nil {
		return fail.named(name)
	}
	after, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if after.Key != chromiumKeyName {
		return failAssert(name, fmt.Sprintf("page received key %q, want %q", after.Key, chromiumKeyName))
	}
	if fail := s.expectRepaint(ctx, sess, pid, shotBefore); fail != nil {
		return fail.named(name)
	}
	return pass(name, fmt.Sprintf("named key %s arrived in the DOM as %q and the window repainted", uiNamedKey, after.Key))
}

// chromiumGesture is the shared body of the Chromium checks whose shape is
// "read state, aim at an accessible element, act, re-read state, prove the
// window repainted". verify returns "" when the state moved as required, or a
// human-readable reason it did not.
func (s *Suite) chromiumGesture(
	ctx context.Context,
	sess *mcp.ClientSession,
	pid int,
	name, target string,
	gesture func(*api.AccessibilityElement) api.ComputerUseInput,
	verify func(before, after fixtureState) string,
) CheckResult {
	before, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	el, fail := s.locateElement(ctx, sess, pid, target)
	if fail != nil {
		return fail.named(name)
	}
	shotBefore, fail := s.capture(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if fail := s.act(ctx, sess, gesture(el)); fail != nil {
		return fail.named(name)
	}
	after, fail := s.chromiumState(ctx, sess, pid)
	if fail != nil {
		return fail.named(name)
	}
	if reason := verify(before, after); reason != "" {
		return failAssert(name, reason)
	}
	if fail := s.expectRepaint(ctx, sess, pid, shotBefore); fail != nil {
		return fail.named(name)
	}
	return pass(name, fmt.Sprintf("gesture on %q changed the page's own state and repainted the window", target))
}

// ---------------------------------------------------------------------------
// Tool helpers
// ---------------------------------------------------------------------------

// locateApp finds a running application whose reported name contains name.
// The comparison is a case-insensitive substring: Cua reports the application
// name from WM_CLASS, GTK capitalises what it derives from prgname, and
// Chromium may append to its own. The fixture names are distinctive enough that
// a substring cannot collide with an unrelated window.
func (s *Suite) locateApp(ctx context.Context, sess *mcp.ClientSession, title string) (api.ApplicationInfo, *terminalFail) {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionListApplications,
	}, &out); err != nil {
		return api.ApplicationInfo{}, &terminalFail{transport: true, detail: "computer_use list_applications call failed"}
	}
	if out.Code != "" {
		return api.ApplicationInfo{}, &terminalFail{detail: fmt.Sprintf("list_applications reported %s", out.Code)}
	}
	for _, app := range out.Applications {
		if strings.Contains(strings.ToLower(app.Name), strings.ToLower(title)) {
			return app, nil
		}
	}
	return api.ApplicationInfo{}, &terminalFail{detail: fmt.Sprintf(
		"no application named %q among the %d listed applications; the fixture is not running",
		title, len(out.Applications))}
}

// focus raises the fixture window so subsequent absolute-coordinate gestures
// reach it rather than whatever the window manager last stacked on top.
func (s *Suite) focus(ctx context.Context, sess *mcp.ClientSession, pid int) *terminalFail {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionFocusApplication, PID: &pid,
	}, &out); err != nil {
		return &terminalFail{transport: true, detail: "computer_use focus_application call failed"}
	}
	if out.Code != "" {
		return &terminalFail{detail: fmt.Sprintf("focus_application reported %s", out.Code)}
	}
	return nil
}

// accessibility returns a FRESH accessibility tree for the given window. It is
// never cached: the whole point of the post-gesture query is that it re-walks
// the live tree, so a stale snapshot would turn every assertion vacuous.
func (s *Suite) accessibility(ctx context.Context, sess *mcp.ClientSession, pid int) ([]api.AccessibilityElement, *terminalFail) {
	maxElements := uiMaxElements
	maxDepth := uiMaxTreeDepth
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionAccessibility,
		Scope:  api.ScopeWindow, PID: &pid,
		MaxElements: &maxElements, MaxDepth: &maxDepth,
	}, &out); err != nil {
		return nil, &terminalFail{transport: true, detail: "computer_use accessibility call failed"}
	}
	if out.Code != "" {
		return nil, &terminalFail{detail: fmt.Sprintf("accessibility reported %s", out.Code)}
	}
	if len(out.Elements) == 0 {
		return nil, &terminalFail{detail: "accessibility returned an empty tree for the fixture window"}
	}
	return out.Elements, nil
}

// locateElement queries a fresh tree and returns the element whose name matches
// target, preferring an exact match. Returning the element (with its geometry)
// rather than an element_index is deliberate: aiming at raw coordinates is what
// puts the reported geometry itself under test, whereas element_index would let
// the server resolve the target internally and hide a geometry regression.
func (s *Suite) locateElement(ctx context.Context, sess *mcp.ClientSession, pid int, target string) (*api.AccessibilityElement, *terminalFail) {
	elements, fail := s.accessibility(ctx, sess, pid)
	if fail != nil {
		return nil, fail
	}
	el, ok := findElement(elements, target)
	if !ok {
		return nil, &terminalFail{detail: fmt.Sprintf(
			"no accessible element named %q among %d elements", target, len(elements))}
	}
	if el.Width <= 0 || el.Height <= 0 {
		return nil, &terminalFail{detail: fmt.Sprintf(
			"accessible element %q has a degenerate %dx%d rectangle; nothing can be aimed at it",
			target, el.Width, el.Height)}
	}
	return el, nil
}

// expectAccessibleText re-queries the tree and requires some element's name to
// contain want. This is the accessibility half of the GTK assertions: it proves
// the AT-SPI bridge published the app's new state, independently of the JSON
// file the app wrote.
func (s *Suite) expectAccessibleText(ctx context.Context, sess *mcp.ClientSession, pid int, want string) *terminalFail {
	elements, fail := s.accessibility(ctx, sess, pid)
	if fail != nil {
		return fail
	}
	if !anyElementContains(elements, want) {
		return &terminalFail{detail: fmt.Sprintf(
			"no accessible element reports %q after the gesture (searched %d elements)", want, len(elements))}
	}
	return nil
}

// capture takes a window-scoped screenshot. A capture that is empty or
// implausibly small is treated as a failure in its own right: an all-but-empty
// PNG is what a window that never got a backing pixmap produces, and accepting
// it would let the repaint comparison below pass on two equally broken frames.
func (s *Suite) capture(ctx context.Context, sess *mcp.ClientSession, pid int) (string, *terminalFail) {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionCapture, Scope: api.ScopeWindow, PID: &pid,
	}, &out); err != nil {
		return "", &terminalFail{transport: true, detail: "computer_use capture call failed"}
	}
	if out.Code != "" {
		return "", &terminalFail{detail: fmt.Sprintf("capture reported %s", out.Code)}
	}
	if len(out.ImageBase64) < uiCaptureMinimum {
		return "", &terminalFail{detail: fmt.Sprintf(
			"window capture returned only %d bytes; the window has no usable pixels", len(out.ImageBase64))}
	}
	return out.ImageBase64, nil
}

// expectRepaint proves the gesture reached the PIXELS, not just the DOM. Every
// Chromium gesture rewrites the page's visible state block, so the window's
// pixels must differ afterwards. An identical capture means the compositor
// handed back the same frame twice -- the appliance would be showing a human
// operator (and any screenshot-driven model) a screen that no longer matches
// reality, which is the failure the accessibility tree alone cannot detect.
func (s *Suite) expectRepaint(ctx context.Context, sess *mcp.ClientSession, pid int, before string) *terminalFail {
	after, fail := s.capture(ctx, sess, pid)
	if fail != nil {
		return fail
	}
	if after == before {
		return &terminalFail{detail: "the window's pixels are byte-identical after the gesture; nothing repainted"}
	}
	return nil
}

// act performs one computer_use gesture and then waits a fixed settle interval.
// The wait goes through the public wait action rather than a local sleep on
// purpose: the delay must be observed by the same session that issued the
// gesture, so a paused or wedged session surfaces here instead of being papered
// over by the client sleeping happily on its own.
func (s *Suite) act(ctx context.Context, sess *mcp.ClientSession, in api.ComputerUseInput) *terminalFail {
	var out api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, in, &out); err != nil {
		return &terminalFail{transport: true, detail: fmt.Sprintf("computer_use %s call failed", in.Action)}
	}
	if out.Code != "" {
		return &terminalFail{detail: fmt.Sprintf("computer_use %s reported %s", in.Action, out.Code)}
	}

	settle := uiSettleMs
	var wait api.ComputerUseOutput
	if err := s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{
		Action: api.ActionWait, DurationMs: &settle,
	}, &wait); err != nil {
		return &terminalFail{transport: true, detail: "computer_use wait call failed"}
	}
	if wait.Code != "" {
		return &terminalFail{detail: fmt.Sprintf("computer_use wait reported %s", wait.Code)}
	}
	return nil
}

// gtkState reads the GTK fixture's own state document through the public
// read_file tool. Using read_file rather than terminal keeps the gate on the
// declared file surface and means a regression in file reads is caught here
// too, but the real reason this exists is independence: the file is written by
// the application, so it cannot be satisfied by a computer_use implementation
// that reports success without doing anything.
func (s *Suite) gtkState(ctx context.Context, sess *mcp.ClientSession) (fixtureState, *terminalFail) {
	var out api.ReadFileOutput
	if err := s.callInto(ctx, sess, api.ToolReadFile, api.ReadFileInput{Path: gtkStatePath}, &out); err != nil {
		return fixtureState{}, &terminalFail{transport: true, detail: "read_file call for the GTK state failed"}
	}
	if out.Code != "" {
		return fixtureState{}, &terminalFail{detail: fmt.Sprintf(
			"read_file of the GTK fixture state reported %s; the fixture may not be running", out.Code)}
	}
	state, ok := parseFixtureState(out.Content)
	if !ok {
		return fixtureState{}, &terminalFail{detail: "the GTK fixture state file is not valid state JSON"}
	}
	return state, nil
}

// chromiumState reads the page's state out of a FRESH accessibility tree. The
// page renders its serialized state into a visible element, so this is the
// same text a human would read off the screen -- obtained without CDP, and
// therefore without asking the browser to vouch for itself.
func (s *Suite) chromiumState(ctx context.Context, sess *mcp.ClientSession, pid int) (fixtureState, *terminalFail) {
	elements, fail := s.accessibility(ctx, sess, pid)
	if fail != nil {
		return fixtureState{}, fail
	}
	for _, el := range elements {
		if state, ok := parseFixtureState(el.Name); ok {
			return state, nil
		}
	}
	return fixtureState{}, &terminalFail{detail: fmt.Sprintf(
		"the page's rendered state text is not present in the accessibility tree (searched %d elements)", len(elements))}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// stateJSONMarker is the leading fragment of both fixtures' state documents.
// Requiring it before attempting a decode keeps chromiumState from mistaking an
// unrelated JSON-looking accessible name for the fixture's state.
const stateJSONMarker = `"clicks"`

// parseFixtureState extracts a fixtureState from text that CONTAINS the state
// document. The Chromium window title carries the same JSON with a prefix, and
// accessible names may be padded, so the object is located by its braces rather
// than requiring the whole string to be JSON.
func parseFixtureState(text string) (fixtureState, bool) {
	if !strings.Contains(text, stateJSONMarker) {
		return fixtureState{}, false
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return fixtureState{}, false
	}
	var state fixtureState
	if err := json.Unmarshal([]byte(text[start:end+1]), &state); err != nil {
		return fixtureState{}, false
	}
	return state, true
}

// findElement returns the element named target, preferring an exact name match
// over a substring one so that "Drag source" cannot be satisfied by an element
// merely mentioning it.
func findElement(elements []api.AccessibilityElement, target string) (*api.AccessibilityElement, bool) {
	for i := range elements {
		if elements[i].Name == target {
			return &elements[i], true
		}
	}
	for i := range elements {
		if strings.Contains(elements[i].Name, target) {
			return &elements[i], true
		}
	}
	return nil, false
}

// anyElementContains reports whether any element's name contains want.
func anyElementContains(elements []api.AccessibilityElement, want string) bool {
	for _, el := range elements {
		if strings.Contains(el.Name, want) {
			return true
		}
	}
	return false
}

// center returns the midpoint of an element's reported rectangle. Gestures aim
// at the centre rather than the origin because a control's origin often lies on
// its border, where a one-pixel geometry error is enough to miss entirely --
// and a gate that is flaky about geometry teaches operators to ignore it.
func center(el *api.AccessibilityElement) (int, int) {
	return el.X + el.Width/2, el.Y + el.Height/2
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

// ---------------------------------------------------------------------------
// Known limits of the accessibility half
// ---------------------------------------------------------------------------
//
// api.AccessibilityElement carries only role, name, and geometry -- there is no
// value or state field. That shapes what "asserted via accessibility" can mean
// here, and the choices above are deliberate rather than incidental:
//
//   - Counters and label text are asserted by NAME, because both fixtures put
//     their state into text that becomes an accessible name.
//   - Scroll position is asserted by GEOMETRY (a known row must move), because
//     neither toolkit exposes a scroll offset as a name and the element type has
//     no value field to read one from.
//   - The GTK entry's CONTENT is the one thing accessibility cannot confirm:
//     ATK exposes entry text through AtkText, which this element type does not
//     model, and the fixture pins the entry's accessible name to "Text input".
//     That check therefore asserts the entry survives in a fresh tree and reads
//     the typed string from the application's JSON. If the entry ever needs a
//     pure-accessibility assertion, the fixture -- not this file -- is what has
//     to change: it would have to mirror its entry text into an accessible name,
//     as the Chromium fixture already does via its rendered state block.
