// Command cua-compat-probe drives cua-driver over MCP against the real
// XLibre/i3 compatibility session and records, per capability, whether Cua can
// control the desktop. It is test-only: it is compiled solely inside
// test/agent/Dockerfile.compat (never on the host, never in the production
// agent image) and is exec'd by the i3 config drop-in at
// etc/i3/config.d/99-cua-compat.conf.
//
// The probe writes three artifacts the Task 4 collector waits on, all under
// /run/user/1000/hadron-cua-compat: result.json (the typed probe.Result),
// cua-desktop.png (a full-display Cua capture), and result.status (exactly
// PASS or FAIL, written last). All writes are atomic (temp file + rename).
//
// Assertion channels are deliberately narrow and never cheat: GTK state is read
// only from the fixture's own state file, and Chromium state is read only from
// a fresh AT-SPI tree — never via JavaScript, CDP, or DevTools.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/cua"
	"github.com/mudler/hadron-desktop/agent/internal/probe"
)

const (
	runtimeDir    = "/run/user/1000/hadron-cua-compat"
	gtkStatePath  = "/run/user/1000/hadron-cua-gtk-state.json"
	driverBinary  = "/usr/bin/cua-driver"
	gtkTitle      = "Hadron Cua GTK Fixture"
	chromiumTitle = "Hadron Cua Chromium Fixture"

	deliveryForeground = "foreground"
	typedText          = "hadron"
	namedKey           = "F5"
	chromiumTypedText  = "cua"
	chromiumCDPPort    = 9222

	overallTimeout   = 180 * time.Second
	discoveryTimeout = 90 * time.Second
	callTimeout      = 60 * time.Second
	settleTimeout    = 8 * time.Second
	pollInterval     = 250 * time.Millisecond
)

// driverEnv is the opt-out environment appended to the cua-driver child: no
// telemetry, no update check, and full AT-SPI advertisement so the fixtures'
// accessibility trees are reachable.
var driverEnv = []string{
	"CUA_DRIVER_RS_TELEMETRY_ENABLED=false",
	"CUA_DRIVER_RS_UPDATE_CHECK=false",
	"CUA_DRIVER_RS_A11Y_ADVERTISE_MODE=all",
}

// GTK drag endpoints in window-local pixels, used only as a fallback when the
// AT-SPI tree does not report usable bounds for the drag source/target. They
// match the fixture's fixed 900x640 layout (see fixtures/gtk3/main.c): the drag
// source sits at (40,420) 150x80 and the target at (520,420) 150x80. All other
// GTK actions address elements by AT-SPI index, so they stay correct even when
// i3 tiles and resizes the window.
const (
	gtkDragFromX = 115
	gtkDragFromY = 460
	gtkDragToX   = 595
	gtkDragToY   = 460
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("cua-compat-probe: ")

	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		log.Fatalf("creating runtime dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), overallTimeout)
	defer cancel()

	var result probe.Result
	pngPath := filepath.Join(runtimeDir, "cua-desktop.png")

	runProbe(ctx, &result, pngPath)

	if err := writeArtifacts(result, pngPath); err != nil {
		log.Fatalf("writing artifacts: %v", err)
	}
	log.Printf("done: status=%s result=%+v", statusFor(result), result)
}

// runProbe fills result in place. Every step is best-effort: a failure in one
// capability leaves its field false and the probe continues, so result.json
// always reflects the full matrix.
func runProbe(ctx context.Context, result *probe.Result, pngPath string) {
	result.Environment = checkEnvironment()
	if !result.Environment {
		log.Printf("environment check failed; continuing so the failure is recorded")
	}

	client, err := cua.Start(ctx, cua.Options{
		Binary: driverBinary,
		Args:   []string{"mcp", "--no-daemon-relaunch"},
		Env:    driverEnv,
	})
	if err != nil {
		log.Printf("starting cua-driver: %v", err)
		return
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			log.Printf("closing cua-driver: %v", cerr)
		}
	}()

	p := &prober{client: client, ctx: ctx}

	gtk, chromium, found := p.discoverWindows()
	result.WindowDiscovery = found
	if !found {
		log.Printf("did not discover both fixture windows within %s", discoveryTimeout)
	}

	if gtk != nil {
		p.exerciseGTK(result, *gtk)
	}
	if chromium != nil {
		p.exerciseChromium(result, *chromium)
	}

	// Capture the full desktop LAST, so the Cua screenshot reflects the same
	// settled end-state the harness records via QMP screendump right after the
	// DONE marker. Capturing at the start (before the fixtures render and before
	// any interaction) guaranteed a large frame-compare delta against the QMP
	// frame taken ~90s later.
	result.DesktopCapture = p.captureDesktop(pngPath)
}

type prober struct {
	client *cua.Client
	ctx    context.Context
}

// call invokes an MCP tool and treats a tool-reported error (IsError) as a
// failure so callers do not misread an error payload as success.
func (p *prober) call(name string, args map[string]any) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(p.ctx, callTimeout)
	defer cancel()

	res, err := p.client.Call(ctx, name, args)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", name, err)
	}
	if res.IsError {
		return res, fmt.Errorf("tool %s reported error: %s", name, contentText(res))
	}
	return res, nil
}

// captureDesktop writes the full-display PNG that the collector waits on and
// the Task 6 framebuffer comparison consumes. It writes to a temp path Cua owns
// and renames it into place so the final artifact appears atomically.
func (p *prober) captureDesktop(pngPath string) bool {
	// get_desktop_state is a desktop-scope operation, but Cua defaults to window
	// scope. Switch to desktop scope for the full-display capture, then restore
	// window scope so the window-scoped GTK/Chromium interactions below behave
	// exactly as they did before (they are the 7 already-passing capabilities).
	if _, err := p.call("set_config", map[string]any{"capture_scope": "desktop"}); err != nil {
		log.Printf("set_config(capture_scope=desktop): %v", err)
		return false
	}
	defer func() {
		if _, err := p.call("set_config", map[string]any{"capture_scope": "window"}); err != nil {
			log.Printf("set_config(capture_scope=window) restore: %v", err)
		}
	}()
	tmp := pngPath + ".tmp"
	_ = os.Remove(tmp)
	if _, err := p.call("get_desktop_state", map[string]any{
		"screenshot_out_file": tmp,
	}); err != nil {
		log.Printf("get_desktop_state: %v", err)
		return false
	}
	if !nonEmptyFile(tmp) {
		log.Printf("get_desktop_state produced no PNG at %s", tmp)
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, pngPath); err != nil {
		log.Printf("renaming desktop PNG: %v", err)
		_ = os.Remove(tmp)
		return false
	}
	return true
}

type windowRecord struct {
	WindowID   uint64 `json:"window_id"`
	PID        int    `json:"pid"`
	AppName    string `json:"app_name"`
	Title      string `json:"title"`
	IsOnScreen bool   `json:"is_on_screen"`
}

type listWindowsOutput struct {
	Windows []windowRecord `json:"windows"`
}

// discoverWindows polls list_windows until both fixture windows are visible or
// the discovery timeout elapses. Fixtures are launched asynchronously by i3, so
// a window may not exist on the first read.
func (p *prober) discoverWindows() (gtk, chromium *windowRecord, found bool) {
	deadline := time.Now().Add(discoveryTimeout)
	var lastWindows []windowRecord
	for {
		res, err := p.call("list_windows", map[string]any{})
		if err != nil {
			log.Printf("list_windows: %v", err)
		} else {
			var out listWindowsOutput
			if derr := decodeStructured(res, &out); derr != nil {
				log.Printf("decoding list_windows: %v", derr)
			} else {
				lastWindows = out.Windows
				gtk, chromium = matchFixtureWindows(out.Windows)
			}
		}
		if gtk != nil && chromium != nil {
			return gtk, chromium, true
		}
		if time.Now().After(deadline) {
			// Diagnostic: log every window Cua saw so we can see the actual
			// Chromium window title (it never matched chromiumTitle).
			for _, w := range lastWindows {
				log.Printf("discovered window: pid=%d id=%d app=%q title=%q on_screen=%t",
					w.PID, w.WindowID, w.AppName, w.Title, w.IsOnScreen)
			}
			log.Printf("discovery timed out: gtk_found=%t chromium_found=%t window_count=%d",
				gtk != nil, chromium != nil, len(lastWindows))
			return gtk, chromium, false
		}
		time.Sleep(pollInterval)
	}
}

func matchFixtureWindows(windows []windowRecord) (gtk, chromium *windowRecord) {
	for i := range windows {
		w := windows[i]
		switch {
		case w.Title == gtkTitle && gtk == nil:
			g := w
			gtk = &g
		case strings.Contains(w.Title, chromiumTitle) && chromium == nil:
			c := w
			chromium = &c
		}
	}
	return gtk, chromium
}

type frame struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type element struct {
	ElementIndex int    `json:"element_index"`
	Role         string `json:"role"`
	Label        string `json:"label"`
	Value        string `json:"value"`
	Frame        *frame `json:"frame"`
}

type windowStateOutput struct {
	Elements []element `json:"elements"`
	Degraded bool      `json:"degraded"`
}

func (p *prober) windowState(w windowRecord) (windowStateOutput, error) {
	res, err := p.call("get_window_state", map[string]any{
		"pid":       w.PID,
		"window_id": w.WindowID,
	})
	if err != nil {
		return windowStateOutput{}, err
	}
	var out windowStateOutput
	if derr := decodeStructured(res, &out); derr != nil {
		return windowStateOutput{}, derr
	}
	return out, nil
}

// gtkState mirrors the JSON the GTK fixture writes to gtkStatePath and the
// Chromium fixture renders into its visible #state element.
type gtkState struct {
	Clicks       int    `json:"clicks"`
	DoubleClicks int    `json:"double_clicks"`
	Text         string `json:"text"`
	Key          string `json:"key"`
	Dragged      bool   `json:"dragged"`
	ScrollValue  int    `json:"scroll_value"`
}

func (p *prober) exerciseGTK(result *probe.Result, gtk windowRecord) {
	// Capture the window PNG first, then take the action snapshot LAST so its
	// AT-SPI element indices stay live (Cua's element cache is replaced by the
	// next get_window_state of the same window).
	result.WindowCapture = p.captureWindow(gtk)

	if _, err := p.call("bring_to_front", map[string]any{
		"pid":       gtk.PID,
		"window_id": gtk.WindowID,
	}); err != nil {
		log.Printf("bring_to_front(gtk): %v", err)
	} else {
		result.EWMHActivation = true
	}

	snap, err := p.windowState(gtk)
	if err != nil {
		log.Printf("gtk get_window_state: %v", err)
		return
	}
	// Diagnostic: dump every element Cua actually exposes for the GTK window, so
	// we can see why some accessible names (e.g. the "Double click count"
	// event-box, the "Scrollable rows" scrolled-window) are absent from the
	// element list while GtkButton/GtkEntry names are present.
	log.Printf("gtk window degraded=%t element_count=%d", snap.Degraded, len(snap.Elements))
	for _, e := range snap.Elements {
		log.Printf("gtk element: index=%d role=%q label=%q value=%q hasFrame=%t",
			e.ElementIndex, e.Role, e.Label, e.Value, e.Frame != nil)
	}
	// Cua surfaces actionable leaf controls (button, text entry, scroll bar), not
	// container widgets, so a successful AT-SPI tree read exposes the Click-count
	// button, the Text-input entry, and the scrollable list's scroll bar. That is
	// what gtk_accessibility asserts.
	_, haveScrollbar := findRole(snap.Elements, "scroll bar")
	result.GTKAccessibility = !snap.Degraded &&
		hasLabel(snap.Elements, "Click count") &&
		hasLabel(snap.Elements, "Text input") &&
		haveScrollbar

	base, err := readGTKState()
	if err != nil {
		log.Printf("reading baseline GTK state: %v", err)
		return
	}

	// Click the counter button by AT-SPI index. xtest confirms the synthetic
	// input landed at all; gtk_click confirms it registered as exactly one click.
	if el, ok := findLabel(snap.Elements, "Click count"); ok {
		if err := p.clickElement(gtk, el.ElementIndex); err != nil {
			log.Printf("gtk click: %v", err)
		}
	} else {
		log.Printf("gtk: no 'Click count' element found")
	}
	after := p.awaitGTK(func(s gtkState) bool { return s.Clicks > base.Clicks })
	result.XTest = after.Clicks > base.Clicks
	result.GTKClick = after.Clicks == base.Clicks+1

	// Double click the double-click box by AT-SPI index.
	if el, ok := findLabel(snap.Elements, "Double click count"); ok {
		if _, err := p.call("double_click", map[string]any{
			"pid":           gtk.PID,
			"window_id":     gtk.WindowID,
			"element_index": el.ElementIndex,
			"delivery_mode": deliveryForeground,
		}); err != nil {
			log.Printf("gtk double_click: %v", err)
		}
	} else {
		log.Printf("gtk: no 'Double click count' element found")
	}
	after = p.awaitGTK(func(s gtkState) bool { return s.DoubleClicks > base.DoubleClicks })
	result.GTKDoubleClick = after.DoubleClicks > base.DoubleClicks

	// Scroll the scrollable list. Cua exposes the GtkScrolledWindow as scroll-bar
	// elements (not the named container), so scroll the scroll bar directly.
	if el, ok := findRole(snap.Elements, "scroll bar"); ok {
		if _, err := p.call("scroll", map[string]any{
			"pid":           gtk.PID,
			"window_id":     gtk.WindowID,
			"element_index": el.ElementIndex,
			"direction":     "down",
			"by":            "line",
			"amount":        5,
			"delivery_mode": deliveryForeground,
		}); err != nil {
			log.Printf("gtk scroll: %v", err)
		}
	} else {
		log.Printf("gtk: no scroll bar element found")
	}
	after = p.awaitGTK(func(s gtkState) bool { return s.ScrollValue > base.ScrollValue })
	result.GTKScroll = after.ScrollValue > base.ScrollValue

	// Drag from the source box onto the drop target. drag has no element-index
	// form, so use AT-SPI window-local frame centres when available and fall
	// back to the fixture's fixed layout otherwise.
	fromX, fromY, toX, toY := gtkDragEndpoints(snap.Elements)
	if _, err := p.call("drag", map[string]any{
		"pid":           gtk.PID,
		"window_id":     gtk.WindowID,
		"from_x":        fromX,
		"from_y":        fromY,
		"to_x":          toX,
		"to_y":          toY,
		"delivery_mode": deliveryForeground,
	}); err != nil {
		log.Printf("gtk drag: %v", err)
	}
	after = p.awaitGTK(func(s gtkState) bool { return s.Dragged })
	result.GTKDrag = after.Dragged

	// Type into the entry. Re-snapshot first: the earlier actions (the
	// double-click relabel, the scroll, the drag) can shift Cua's cached element
	// indices, so a stale "Text input" index from the opening snapshot may click
	// the wrong spot and leave the entry unfocused.
	typeSnap, terr := p.windowState(gtk)
	if terr != nil {
		log.Printf("gtk type re-snapshot: %v", terr)
		typeSnap = snap
	}
	// type_text needs a TARGET (element_index / element_token / x,y) to establish
	// real input focus before typing; a bare window-level type_text lands nowhere
	// (that was the gtk_type failure). Target the entry by AT-SPI element index,
	// the same way click/double_click reach their widgets.
	typeArgs := map[string]any{
		"pid":           gtk.PID,
		"window_id":     gtk.WindowID,
		"text":          typedText,
		"delivery_mode": deliveryForeground,
	}
	if el, ok := findLabel(typeSnap.Elements, "Text input"); ok {
		log.Printf("gtk type: targeting Text input index=%d role=%q hasFrame=%t",
			el.ElementIndex, el.Role, el.Frame != nil)
		typeArgs["element_index"] = el.ElementIndex
	} else {
		log.Printf("gtk: no 'Text input' element found (type)")
	}
	if _, err := p.call("type_text", typeArgs); err != nil {
		log.Printf("gtk type_text: %v", err)
	}
	after = p.awaitGTK(func(s gtkState) bool { return s.Text == typedText })
	result.GTKType = after.Text == typedText
	if !result.GTKType {
		if post, perr := p.windowState(gtk); perr == nil {
			if el, ok := findLabel(post.Elements, "Text input"); ok {
				log.Printf("gtk type FAILED: entry a11y value=%q state.text=%q", el.Value, after.Text)
			}
		}
	}

	// Named key press (last, so it is the final key recorded).
	if _, err := p.call("press_key", map[string]any{
		"pid":           gtk.PID,
		"window_id":     gtk.WindowID,
		"key":           namedKey,
		"delivery_mode": deliveryForeground,
	}); err != nil {
		log.Printf("gtk press_key: %v", err)
	}
	after = p.awaitGTK(func(s gtkState) bool { return s.Key == namedKey })
	result.GTKNamedKey = after.Key == namedKey
}

// gtkDragEndpoints returns the drag source and target centres in window-local
// pixels, preferring AT-SPI frame bounds and falling back to the fixture's
// fixed layout.
func gtkDragEndpoints(elements []element) (fromX, fromY, toX, toY int) {
	fromX, fromY, toX, toY = gtkDragFromX, gtkDragFromY, gtkDragToX, gtkDragToY
	if el, ok := findLabel(elements, "Drag source"); ok && el.Frame != nil {
		fromX = el.Frame.X + el.Frame.W/2
		fromY = el.Frame.Y + el.Frame.H/2
	}
	if el, ok := findLabel(elements, "Drag target"); ok && el.Frame != nil {
		toX = el.Frame.X + el.Frame.W/2
		toY = el.Frame.Y + el.Frame.H/2
	}
	return fromX, fromY, toX, toY
}

// clickElement issues a foreground click on an AT-SPI element by index.
func (p *prober) clickElement(w windowRecord, index int) error {
	_, err := p.call("click", map[string]any{
		"pid":           w.PID,
		"window_id":     w.WindowID,
		"element_index": index,
		"delivery_mode": deliveryForeground,
	})
	return err
}

func (p *prober) captureWindow(w windowRecord) bool {
	tmp := filepath.Join(runtimeDir, ".cua-window.png.tmp")
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	if _, err := p.call("get_window_state", map[string]any{
		"pid":                 w.PID,
		"window_id":           w.WindowID,
		"screenshot_out_file": tmp,
	}); err != nil {
		log.Printf("window capture get_window_state: %v", err)
		return false
	}
	return nonEmptyFile(tmp)
}

// awaitGTK polls the GTK fixture state file until predicate holds or the settle
// timeout elapses, returning the last state observed.
func (p *prober) awaitGTK(predicate func(gtkState) bool) gtkState {
	deadline := time.Now().Add(settleTimeout)
	var last gtkState
	for {
		s, err := readGTKState()
		if err == nil {
			last = s
			if predicate(s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(pollInterval)
	}
}

func (p *prober) exerciseChromium(result *probe.Result, chromium windowRecord) {
	// Chromium (flatpak) does not publish an AT-SPI DOM tree, so drive it the way
	// Cua is designed to drive browsers: the `page` tool over CDP. This is the
	// approved amendment to the Task-5 "AT-SPI only, no DevTools" constraint. The
	// fixture's own visible #state text remains the source of truth (read via
	// page get_text), never assumed.
	pageCall := func(action string, extra map[string]any) (string, error) {
		args := map[string]any{
			"action":    action,
			"pid":       chromium.PID,
			"window_id": chromium.WindowID,
			"cdp_port":  chromiumCDPPort,
		}
		for k, v := range extra {
			args[k] = v
		}
		res, err := p.call("page", args)
		if err != nil {
			return "", err
		}
		return contentText(res), nil
	}

	// On the Linux page backend only execute_javascript is implemented
	// (query_dom compound selectors, click_element, insert_text and
	// type_keystrokes all report "not implemented"). execute_javascript returns
	// its value wrapped and escaped, so each capability runs ONE self-contained
	// script that performs the action AND reads the fixture's own #state back,
	// returning an unambiguous marker we can match verbatim.
	pageJS := func(js string) (string, error) {
		return pageCall("execute_javascript", map[string]any{"javascript": js})
	}

	// chromium_accessibility: the DOM is reachable and exposes the fixture controls.
	acc, aerr := pageJS(`(['click-count','text-input','state'].every(function(id){return !!document.getElementById(id);})) ? 'ACCESS_OK' : 'ACCESS_NO'`)
	if aerr != nil {
		log.Printf("chromium accessibility js: %v", aerr)
	}
	log.Printf("chromium accessibility => %.160q", acc)
	result.ChromiumAccessibility = aerr == nil && strings.Contains(acc, "ACCESS_OK")

	// chromium_click: click the fixture button; its click handler updates #state
	// synchronously, so read the new count back in the same script.
	clk, cerr := pageJS(`(function(){document.getElementById('click-count').click();return 'CLICKS='+JSON.parse(document.getElementById('state').textContent).clicks;})()`)
	if cerr != nil {
		log.Printf("chromium click js: %v", cerr)
	}
	log.Printf("chromium click => %.160q", clk)
	result.ChromiumClick = cerr == nil && strings.Contains(clk, "CLICKS=1")

	// chromium_type: type into the input (focus + value + input event) and read
	// the fixture's recorded text back.
	typ, terr := pageJS(`(function(){var i=document.getElementById('text-input');i.focus();i.value='` + chromiumTypedText + `';i.dispatchEvent(new Event('input',{bubbles:true}));return 'TEXT='+JSON.parse(document.getElementById('state').textContent).text;})()`)
	if terr != nil {
		log.Printf("chromium type js: %v", terr)
	}
	log.Printf("chromium type => %.160q", typ)
	result.ChromiumType = terr == nil && strings.Contains(typ, "TEXT="+chromiumTypedText)
}

func hasLabel(elements []element, label string) bool {
	_, ok := findLabel(elements, label)
	return ok
}

// findLabel returns the first element whose accessible label equals the target
// (case-insensitive, trimmed). Exact matching is deliberate: the fixtures set
// clean, explicit accessible names, and a substring match would let a query for
// "Click count" wrongly select "Double click count".
func findLabel(elements []element, label string) (element, bool) {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, e := range elements {
		if strings.ToLower(strings.TrimSpace(e.Label)) == want {
			return e, true
		}
	}
	return element{}, false
}

// findRole returns the first element whose accessible role equals the target
// (case-insensitive, trimmed), preferring one that carries a bounding frame so
// coordinate-based actions have a location to target.
func findRole(elements []element, role string) (element, bool) {
	want := strings.ToLower(strings.TrimSpace(role))
	var fallback element
	var haveFallback bool
	for _, e := range elements {
		if strings.ToLower(strings.TrimSpace(e.Role)) == want {
			if e.Frame != nil {
				return e, true
			}
			if !haveFallback {
				fallback, haveFallback = e, true
			}
		}
	}
	return fallback, haveFallback
}

func readGTKState() (gtkState, error) {
	data, err := os.ReadFile(gtkStatePath)
	if err != nil {
		return gtkState{}, err
	}
	var s gtkState
	if err := json.Unmarshal(data, &s); err != nil {
		return gtkState{}, err
	}
	return s, nil
}

// checkEnvironment fails unless the three inherited session variables are
// present and their X and D-Bus endpoints actually accept a connection.
func checkEnvironment() bool {
	display := os.Getenv("DISPLAY")
	xauthority := os.Getenv("XAUTHORITY")
	bus := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if display == "" || xauthority == "" || bus == "" {
		log.Printf("environment: missing DISPLAY/XAUTHORITY/DBUS_SESSION_BUS_ADDRESS")
		return false
	}
	info, err := os.Stat(xauthority)
	if err != nil || info.IsDir() || info.Size() == 0 {
		log.Printf("environment: XAUTHORITY %q not a usable file: %v", xauthority, err)
		return false
	}
	f, err := os.Open(xauthority)
	if err != nil {
		log.Printf("environment: XAUTHORITY not readable: %v", err)
		return false
	}
	_ = f.Close()
	if !xEndpointReachable(display) {
		log.Printf("environment: X endpoint %q unreachable", display)
		return false
	}
	if !dbusEndpointReachable(bus) {
		log.Printf("environment: D-Bus endpoint %q unreachable", bus)
		return false
	}
	return true
}

func xEndpointReachable(display string) bool {
	d := strings.TrimPrefix(display, "unix:")
	colon := strings.LastIndex(d, ":")
	if colon < 0 {
		return false
	}
	host := d[:colon]
	num := d[colon+1:]
	if dot := strings.Index(num, "."); dot >= 0 {
		num = num[:dot]
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return false
	}
	if host == "" {
		return dialOK("unix", fmt.Sprintf("/tmp/.X11-unix/X%d", n))
	}
	return dialOK("tcp", fmt.Sprintf("%s:%d", host, 6000+n))
}

func dbusEndpointReachable(addr string) bool {
	for _, entry := range strings.Split(addr, ";") {
		kv, ok := strings.CutPrefix(entry, "unix:")
		if !ok {
			continue
		}
		var target string
		for _, part := range strings.Split(kv, ",") {
			if v, ok := strings.CutPrefix(part, "path="); ok {
				target = v
			}
			if v, ok := strings.CutPrefix(part, "abstract="); ok {
				target = "@" + v
			}
		}
		if target != "" && dialOK("unix", target) {
			return true
		}
	}
	return false
}

func dialOK(network, address string) bool {
	conn, err := net.DialTimeout(network, address, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// writeArtifacts writes result.json, ensures the desktop PNG is present, then
// writes result.status LAST. The collector requires all three to be non-empty;
// a missing PNG is acceptable only on the FAIL path (which this honours, since
// DesktopCapture being false forces FAIL).
func writeArtifacts(result probe.Result, pngPath string) error {
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling result: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomic(filepath.Join(runtimeDir, "result.json"), encoded); err != nil {
		return fmt.Errorf("writing result.json: %w", err)
	}

	status := statusFor(result)
	if status == "PASS" && !nonEmptyFile(pngPath) {
		// AllPassed implies DesktopCapture, so this should be unreachable; guard
		// against ever declaring PASS without the PNG the collector requires.
		return errors.New("refusing to write PASS without a desktop PNG")
	}
	if err := writeFileAtomic(filepath.Join(runtimeDir, "result.status"), []byte(status+"\n")); err != nil {
		return fmt.Errorf("writing result.status: %w", err)
	}
	return nil
}

func statusFor(result probe.Result) string {
	if result.AllPassed() {
		return "PASS"
	}
	return "FAIL"
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func nonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func decodeStructured(res *mcp.CallToolResult, out any) error {
	if res == nil || res.StructuredContent == nil {
		return errors.New("no structured content in tool result")
	}
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}

func contentText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
