package api

import (
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// browser
// ---------------------------------------------------------------------------

// BrowserAction selects the operation the browser tool performs.
//
// The browser tool exists because the desktop accessibility path does not
// cover web content: flatpak Chromium publishes no AT-SPI DOM tree, so a
// window-scoped computer_use accessibility query returns a single node and
// there is nothing to locate a link by name with. That leaves screen
// coordinates, which encode window geometry, viewport width, responsive
// breakpoints, scroll offset, and banner state -- and which fail by clicking
// something else rather than by erroring.
//
// Every targeting action here takes a Ref from a Snapshot of the page's own
// accessibility information instead. See
// docs/superpowers/specs/2026-07-20-browser-tool-amendment.md.
type BrowserAction string

const (
	BrowserNavigate BrowserAction = "navigate"
	BrowserSnapshot BrowserAction = "snapshot"
	BrowserClick    BrowserAction = "click"
	BrowserType     BrowserAction = "type"
	BrowserPress    BrowserAction = "press"
	BrowserScroll   BrowserAction = "scroll"
	BrowserBack     BrowserAction = "back"
	BrowserText     BrowserAction = "text"
)

// BrowserActions is the closed, ordered set of browser actions.
var BrowserActions = []BrowserAction{
	BrowserNavigate,
	BrowserSnapshot,
	BrowserClick,
	BrowserType,
	BrowserPress,
	BrowserScroll,
	BrowserBack,
	BrowserText,
}

var validBrowserActions = validSet(BrowserActions)

// BrowserElement is one interactive element from a snapshot. Ref is the handle
// every targeting action uses; it is valid until the page navigates or the next
// snapshot replaces it, after which acting on it is an error rather than a
// click on whatever now occupies that position.
type BrowserElement struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Name string `json:"name,omitempty"`
	// Value is the current value of a form control, when it has one.
	Value string `json:"value,omitempty"`
	// X and Y are viewport-relative CSS pixels, as the page itself sees them.
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`

	// ScreenX and ScreenY are the same point in SCREEN coordinates, which is
	// what computer_use clicks in. The two differ by the browser's chrome --
	// tab strip, URL bar, any notification banner -- plus the window's own
	// position, and the gap is large: 156px on a stock window. Handing a
	// caller only viewport bounds and inviting them to "fall back to
	// computer_use" produced a click 158px above its target.
	//
	// They are omitted when the window geometry could not be read, so a
	// caller can tell "no conversion available" from "the origin is 0,0".
	ScreenX *int `json:"screen_x,omitempty"`
	ScreenY *int `json:"screen_y,omitempty"`
}

// BrowserInput is the single input type for the browser tool. Only the fields
// relevant to Action are meaningful; Validate rejects requests that set fields
// irrelevant to Action, or omit fields Action requires.
type BrowserInput struct {
	Action BrowserAction `json:"action" jsonschema:"the browser operation to perform"`

	// URL is the address navigate loads.
	URL string `json:"url,omitempty" jsonschema:"absolute URL for navigate"`

	// Ref identifies a snapshot element for click, type, text, and scroll.
	Ref string `json:"ref,omitempty" jsonschema:"element ref from a snapshot"`

	// Text is typed into the element Ref names.
	Text string `json:"text,omitempty" jsonschema:"text to type into the referenced element"`

	// Submit asks type to submit the element's form afterwards. It counts as
	// set only when true, so an explicit false is never treated as a field
	// belonging to some other action.
	Submit bool `json:"submit,omitempty" jsonschema:"submit the form after typing"`

	// Key is the key press delivers to the focused element, for example
	// "Enter", "Escape", or "Tab".
	Key string `json:"key,omitempty" jsonschema:"key name for press, e.g. Enter"`

	// Direction and Amount configure scroll. Amount is in CSS pixels.
	Direction ScrollDirection `json:"direction,omitempty" jsonschema:"scroll direction: up, down, left, or right"`
	Amount    *int            `json:"amount,omitempty" jsonschema:"scroll distance in CSS pixels"`

	// MaxElements bounds how many elements a snapshot returns.
	MaxElements *int `json:"max_elements,omitempty" jsonschema:"maximum number of elements to return from a snapshot"`

	// WindowID selects which browser window to act on. It is optional: with a
	// single browser open the tool resolves the window itself, so a caller
	// never has to know a pid. With more than one open, omitting it is an
	// error rather than a silent pick.
	WindowID *int `json:"window_id,omitempty" jsonschema:"browser window to target when more than one is open"`
}

// bField identifies one optional BrowserInput field for per-action
// allow-listing.
type bField uint32

const (
	bURL bField = 1 << iota
	bRef
	bText
	bSubmit
	bKey
	bDirection
	bAmount
	bMaxElements
	bWindowID
)

var bFieldNames = map[bField]string{
	bURL:         "url",
	bRef:         "ref",
	bText:        "text",
	bSubmit:      "submit",
	bKey:         "key",
	bDirection:   "direction",
	bAmount:      "amount",
	bMaxElements: "max_elements",
	bWindowID:    "window_id",
}

// presentFields returns the bitmask of optional fields that are set on in.
func (in BrowserInput) presentFields() bField {
	var f bField
	if in.URL != "" {
		f |= bURL
	}
	if in.Ref != "" {
		f |= bRef
	}
	if in.Text != "" {
		f |= bText
	}
	if in.Submit {
		f |= bSubmit
	}
	if in.Key != "" {
		f |= bKey
	}
	if in.Direction != "" {
		f |= bDirection
	}
	if in.Amount != nil {
		f |= bAmount
	}
	if in.MaxElements != nil {
		f |= bMaxElements
	}
	if in.WindowID != nil {
		f |= bWindowID
	}
	return f
}

// rejectExtraneous returns an error naming the first field set on in that is
// not present in allowed.
func (in BrowserInput) rejectExtraneous(allowed bField) error {
	extra := in.presentFields() &^ allowed
	if extra == 0 {
		return nil
	}
	var names []string
	for bit, name := range bFieldNames {
		if extra&bit != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return fmt.Errorf("action %q does not accept field(s): %v", in.Action, names)
}

// Validate checks that BrowserInput carries a recognized Action, that every
// field set is relevant to that Action, and that Action's required fields are
// present.
func (in BrowserInput) Validate() error {
	if !validBrowserActions[in.Action] {
		return fmt.Errorf("unknown action %q", in.Action)
	}

	switch in.Direction {
	case "", DirectionUp, DirectionDown, DirectionLeft, DirectionRight:
	default:
		return fmt.Errorf("invalid direction %q", in.Direction)
	}

	switch in.Action {
	case BrowserNavigate:
		if err := in.rejectExtraneous(bURL | bWindowID); err != nil {
			return err
		}
		if in.URL == "" {
			return fmt.Errorf("action %q requires url", in.Action)
		}

	case BrowserSnapshot:
		if err := in.rejectExtraneous(bMaxElements | bWindowID); err != nil {
			return err
		}

	case BrowserClick:
		if err := in.rejectExtraneous(bRef | bWindowID); err != nil {
			return err
		}
		if in.Ref == "" {
			return fmt.Errorf("action %q requires ref", in.Action)
		}

	case BrowserType:
		if err := in.rejectExtraneous(bRef | bText | bSubmit | bWindowID); err != nil {
			return err
		}
		if in.Ref == "" {
			return fmt.Errorf("action %q requires ref", in.Action)
		}
		if in.Text == "" {
			return fmt.Errorf("action %q requires text", in.Action)
		}

	case BrowserPress:
		if err := in.rejectExtraneous(bKey | bWindowID); err != nil {
			return err
		}
		if in.Key == "" {
			return fmt.Errorf("action %q requires key", in.Action)
		}

	case BrowserScroll:
		// ref is optional: without one the page scrolls, with one that element
		// does. Real pages put content in scrollable regions -- a chat pane, a
		// table, a sidebar -- that window scrolling never touches.
		if err := in.rejectExtraneous(bRef | bDirection | bAmount | bWindowID); err != nil {
			return err
		}
		if in.Direction == "" {
			return fmt.Errorf("action %q requires direction", in.Action)
		}

	case BrowserBack:
		if err := in.rejectExtraneous(bWindowID); err != nil {
			return err
		}

	case BrowserText:
		// ref is optional: without one, the whole document's text is returned.
		if err := in.rejectExtraneous(bRef | bWindowID); err != nil {
			return err
		}
	}
	return nil
}

// BrowserOutput is the result of a browser call. Which fields are populated
// depends on the action: navigate/click/back report where the page ended up,
// snapshot returns elements, text returns text, type echoes the resulting
// value, and scroll reports the resulting offset.
type BrowserOutput struct {
	ResultMeta

	// URL and Title describe the page after the action settled.
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`

	// Elements is the snapshot result.
	Elements []BrowserElement `json:"elements,omitempty"`

	// Text is the visible text returned by the text action.
	Text string `json:"text,omitempty"`

	// Value is the referenced control's value after a type action.
	Value string `json:"value,omitempty"`

	// ScrollX and ScrollY are the document scroll offsets after a scroll.
	ScrollX *int `json:"scroll_x,omitempty"`
	ScrollY *int `json:"scroll_y,omitempty"`

	// WindowID is the browser window the call acted on, which is useful when
	// the tool resolved it rather than the caller naming it.
	WindowID int `json:"window_id,omitempty"`

	// ClickMethod reports how a click was delivered: "pointer" for a real
	// pointer event at the element's screen position, or "script" for a
	// JavaScript el.click().
	//
	// The distinction is not cosmetic. A scripted click is not a user
	// gesture, so anything gated on user activation -- the Fullscreen API,
	// clipboard access, autoplay -- refuses it, and refuses it SILENTLY: the
	// call reports success and nothing happens. Surfacing which path ran lets
	// a caller understand a click that "worked" but did nothing.
	ClickMethod string `json:"click_method,omitempty"`

	// Truncated reports that a snapshot hit MaxElements and omitted elements.
	Truncated bool `json:"truncated,omitempty"`
}

// BrowserToolDescription is shared by RegisterAll and the gateway so the
// advertised description cannot drift between the stub server and the real one.
const BrowserToolDescription = "Drive web content by ref from the page's own accessibility " +
	"information: navigate, snapshot, click, type, press, scroll, back, text."

// BashToolDescription is shared by RegisterAll and the gateway so the advertised
// description cannot drift between the stub server and the real one.
const BashToolDescription = "Run a shell script to completion. No timeout and no output limit: " +
	"write your own `timeout` into the script if you need one."
