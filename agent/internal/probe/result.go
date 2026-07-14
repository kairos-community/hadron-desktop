// Package probe holds the typed result of the in-session Cua compatibility
// probe. The probe binary (built only inside the compatibility test image at
// test/agent/cmd/cua-compat-probe) fills this struct in by driving cua-driver
// over MCP against the real XLibre/i3 desktop, then serialises it to
// result.json alongside a PASS/FAIL status the Task 4 collector waits on.
package probe

// Result records one boolean per capability the probe exercises. Every field
// serialises to the exact snake_case key the collector, reporter, and Task 7
// gate assert on, so renaming a JSON tag is a breaking change.
type Result struct {
	// Environment is true when the inherited DISPLAY, XAUTHORITY, and
	// DBUS_SESSION_BUS_ADDRESS session variables are present and their X and
	// D-Bus endpoints actually accept a connection.
	Environment bool `json:"environment"`
	// XTest is true when synthetic input delivered through XTEST reaches the
	// real X server (observed as the GTK fixture registering our first click).
	XTest bool `json:"xtest"`
	// WindowDiscovery is true when list_windows surfaces both fixture windows.
	WindowDiscovery bool `json:"window_discovery"`
	// EWMHActivation is true when bring_to_front activates the GTK window.
	EWMHActivation bool `json:"ewmh_activation"`
	// DesktopCapture is true when get_desktop_state writes a non-empty PNG.
	DesktopCapture bool `json:"desktop_capture"`
	// WindowCapture is true when get_window_state writes a non-empty window PNG.
	WindowCapture bool `json:"window_capture"`
	// GTKAccessibility is true when the GTK AT-SPI tree exposes the fixture's
	// labelled controls.
	GTKAccessibility bool `json:"gtk_accessibility"`
	// GTKClick is true when a single click registers exactly one click in the
	// GTK state file.
	GTKClick bool `json:"gtk_click"`
	// GTKDoubleClick is true when a double click registers in the GTK state file.
	GTKDoubleClick bool `json:"gtk_double_click"`
	// GTKDrag is true when a press-drag-release lands on the GTK drop target.
	GTKDrag bool `json:"gtk_drag"`
	// GTKScroll is true when a scroll gesture advances the GTK scroll position.
	GTKScroll bool `json:"gtk_scroll"`
	// GTKType is true when typed text arrives in the GTK entry.
	GTKType bool `json:"gtk_type"`
	// GTKNamedKey is true when a named key press is recorded by the GTK fixture.
	GTKNamedKey bool `json:"gtk_named_key"`
	// ChromiumAccessibility is true when the Chromium AT-SPI tree exposes the
	// fixture's ARIA-labelled controls.
	ChromiumAccessibility bool `json:"chromium_accessibility"`
	// ChromiumClick is true when a click updates the Chromium fixture state,
	// observed through the AT-SPI tree's visible state text.
	ChromiumClick bool `json:"chromium_click"`
	// ChromiumType is true when typed text updates the Chromium fixture state,
	// observed through the AT-SPI tree's visible state text.
	ChromiumType bool `json:"chromium_type"`
}

// AllPassed reports whether every capability field is true.
func (r Result) AllPassed() bool {
	return r.Environment &&
		r.XTest &&
		r.WindowDiscovery &&
		r.EWMHActivation &&
		r.DesktopCapture &&
		r.WindowCapture &&
		r.GTKAccessibility &&
		r.GTKClick &&
		r.GTKDoubleClick &&
		r.GTKDrag &&
		r.GTKScroll &&
		r.GTKType &&
		r.GTKNamedKey &&
		r.ChromiumAccessibility &&
		r.ChromiumClick &&
		r.ChromiumType
}
