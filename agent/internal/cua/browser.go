package cua

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// This file implements the browser tool over Cua's `page` tool.
//
// Only one page action is used: execute_javascript. That is not a stylistic
// choice -- on the Linux page backend it is the only one implemented (query_dom
// compound selectors, click_element, insert_text and type_keystrokes all report
// "not implemented"; see test/agent/cmd/cua-compat-probe/main.go). So every
// browser action is composed as a single self-contained JavaScript program that
// performs the action AND reads the resulting state back in the same round
// trip, which also keeps this tool's coupling to a visibly incomplete backend
// as narrow as it can be.
//
// Design: docs/superpowers/specs/2026-07-20-browser-tool-amendment.md

const (
	toolPage = "page"

	// browserCDPPort is where the appliance's browser exposes the Chrome
	// DevTools Protocol. Chromium's --remote-debugging-port binds 127.0.0.1
	// and the launcher must never pass --remote-debugging-address: CDP is full
	// control of the browser (cookies, authenticated sessions, arbitrary script
	// in any loaded origin), so it stays loopback-only and is never forwarded
	// or proxied off the guest.
	browserCDPPort = 9222

	// defaultSnapshotElements bounds a snapshot a caller did not bound.
	defaultSnapshotElements = 200
	// maxSnapshotElements caps what a caller may ask for, so one call cannot
	// return an unbounded page as a single result.
	maxSnapshotElements = 1000

	// defaultScrollAmount is the scroll distance in CSS pixels when the caller
	// does not give one.
	defaultScrollAmount = 400

	// settleDelay is how long an action that can navigate waits before reading
	// the resulting URL and title. execute_javascript returns as soon as the
	// script does, which for a link click is before the new document exists.
	settleDelay = 1200 * time.Millisecond
)

// browserAppNames are the app_name/title substrings that identify a browser
// window in list_windows output, matched case-insensitively.
var browserAppNames = []string{"chromium", "chrome", "firefox", "brave", "edge", "epiphany", "webkit"}

// pageTarget is the window a browser call acts on. The Cua page tool wants pid,
// window_id and cdp_port on every call; resolving it here is what lets the
// public tool take an optional window_id and never a pid.
type pageTarget struct {
	pid      int
	windowID int64
}

// Browser maps in onto the page calls it requires. It re-validates in so the
// adapter is safe to call directly.
func (a *Adapter) Browser(ctx context.Context, in api.BrowserInput) (api.BrowserOutput, error) {
	if err := in.Validate(); err != nil {
		return browserErr(api.CodeInvalidArgument, err.Error(), false), nil
	}

	// Parse the ref before anything is dispatched. It costs nothing, needs no
	// window, and a caller who mistyped a ref should get that answer rather
	// than whatever the window lookup happens to report first.
	if in.Ref != "" {
		if _, err := refIndex(in.Ref); err != nil {
			return browserErr(api.CodeInvalidArgument, err.Error(), false), nil
		}
	}

	// The browser shares the single seat with computer_use: both ultimately
	// drive the same seat, and a snapshot interleaved with a click would hand
	// out refs for a page that has already moved on.
	select {
	case a.seat <- struct{}{}:
	case <-ctx.Done():
		return browserErr(api.CodeDeadlineExceeded,
			"browser: cancelled while waiting for the computer-use seat: "+ctx.Err().Error(), true), nil
	}
	defer func() { <-a.seat }()

	if err := ctx.Err(); err != nil {
		return browserErr(api.CodeDeadlineExceeded,
			"browser: ctx expired after acquiring the computer-use seat, before dispatch: "+err.Error(), true), nil
	}

	target, meta := a.resolveBrowserWindow(ctx, in)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}

	switch in.Action {
	case api.BrowserNavigate:
		return a.browserNavigate(ctx, target, in)
	case api.BrowserSnapshot:
		return a.browserSnapshot(ctx, target, in)
	case api.BrowserClick:
		return a.browserClick(ctx, target, in)
	case api.BrowserType:
		return a.browserType(ctx, target, in)
	case api.BrowserPress:
		return a.browserPress(ctx, target, in)
	case api.BrowserScroll:
		return a.browserScroll(ctx, target, in)
	case api.BrowserBack:
		return a.browserBack(ctx, target)
	case api.BrowserText:
		return a.browserText(ctx, target, in)
	default:
		// Unreachable given Validate; kept so an action added to the api
		// package without a branch here fails loudly instead of doing nothing.
		return browserErr(api.CodeInvalidArgument,
			fmt.Sprintf("action %q is not mapped to a page call", in.Action), false), nil
	}
}

// ---------------------------------------------------------------------------
// Window resolution
// ---------------------------------------------------------------------------

// resolveBrowserWindow finds the window to act on. With one browser open the
// caller needs to supply nothing; with several, omitting window_id is an error
// rather than a silent pick of whichever happened to sort first.
func (a *Adapter) resolveBrowserWindow(ctx context.Context, in api.BrowserInput) (pageTarget, api.ResultMeta) {
	result, meta := a.invokeAs(ctx, toolListWindows, map[string]any{}, true, "browser")
	if meta.Code != "" {
		return pageTarget{}, meta
	}
	windows, err := decodeWindows(result)
	if err != nil {
		return pageTarget{}, metaFor(api.CodeInternal, "browser: decoding list_windows: "+err.Error(), false)
	}

	// An explicit window_id names any window, browser or not: the caller has
	// said which one, so second-guessing it would only get in the way.
	if in.WindowID != nil {
		want := int64(*in.WindowID)
		for _, w := range windows {
			if w.WindowID == want {
				return pageTarget{pid: w.PID, windowID: w.WindowID}, api.ResultMeta{}
			}
		}
		return pageTarget{}, metaFor(api.CodeNotFound,
			fmt.Sprintf("browser: no window with window_id %d", want), false)
	}

	var candidates []cuaWindow
	for _, w := range windows {
		if isBrowserWindow(w) {
			candidates = append(candidates, w)
		}
	}

	switch len(candidates) {
	case 0:
		return pageTarget{}, metaFor(api.CodeBrowserUnavailable,
			"browser: no browser window is open; start one, then retry", true)
	case 1:
		return pageTarget{pid: candidates[0].PID, windowID: candidates[0].WindowID}, api.ResultMeta{}
	default:
		var ids []string
		for _, w := range candidates {
			ids = append(ids, strconv.FormatInt(w.WindowID, 10))
		}
		return pageTarget{}, metaFor(api.CodeInvalidArgument,
			fmt.Sprintf("browser: %d browser windows are open (window_id %s); pass window_id to choose one",
				len(candidates), strings.Join(ids, ", ")), false)
	}
}

// isBrowserWindow reports whether w looks like a browser window.
//
// It matches on app_name, NOT on the title. A title match looks more generous
// but is actively wrong: a terminal running `flatpak install
// org.chromium.Chromium` has "Chromium" in its title, and misreading that shell
// as a second browser turns auto-resolution into a spurious "pass window_id"
// error. The title is consulted only when app_name is empty, which is the one
// case where it carries the window's identity rather than its contents.
func isBrowserWindow(w cuaWindow) bool {
	haystack := strings.ToLower(w.AppName)
	if strings.TrimSpace(haystack) == "" {
		haystack = strings.ToLower(w.Title)
	}
	for _, name := range browserAppNames {
		if strings.Contains(haystack, name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// pageState is what every action's script reports back about the page.
type pageState struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	ScrollX int    `json:"scroll_x"`
	ScrollY int    `json:"scroll_y"`
	Text    string `json:"text"`
	Value   string `json:"value"`
	// Error is set by the script itself for conditions only it can see, most
	// importantly a ref that no longer resolves.
	Error string `json:"error"`

	Elements  []api.BrowserElement `json:"elements"`
	Truncated bool                 `json:"truncated"`
}

func (a *Adapter) browserNavigate(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	js := fmt.Sprintf(`(function(){location.href=%s;return JSON.stringify({url:location.href,title:document.title});})()`,
		jsString(in.URL))
	if _, meta := a.pageJS(ctx, target, js); meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	// The assignment above only starts the navigation, so the state the script
	// could see is the old page's. Read it again once the new one has settled.
	return a.stateAfterSettle(ctx, target)
}

func (a *Adapter) browserSnapshot(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	limit := defaultSnapshotElements
	if in.MaxElements != nil {
		limit = *in.MaxElements
	}
	if limit <= 0 || limit > maxSnapshotElements {
		limit = maxSnapshotElements
	}

	state, meta := a.pageJS(ctx, target, snapshotJS(limit))
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	return api.BrowserOutput{
		URL:       state.URL,
		Title:     state.Title,
		Elements:  state.Elements,
		Truncated: state.Truncated,
		WindowID:  int(target.windowID),
	}, nil
}

func (a *Adapter) browserClick(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	idx, err := refIndex(in.Ref)
	if err != nil {
		return browserErr(api.CodeInvalidArgument, err.Error(), false), nil
	}
	js := fmt.Sprintf(`(function(){%s el.click();return JSON.stringify({url:location.href,title:document.title});})()`,
		resolveRefJS(idx))
	state, meta := a.pageJS(ctx, target, js)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	if out, stale := staleRef(state, in.Ref); stale {
		return out, nil
	}
	// A click can navigate; report where the page actually ended up.
	return a.stateAfterSettle(ctx, target)
}

func (a *Adapter) browserType(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	idx, err := refIndex(in.Ref)
	if err != nil {
		return browserErr(api.CodeInvalidArgument, err.Error(), false), nil
	}
	// focus + value + an input event, so frameworks listening for input (rather
	// than for keystrokes) see the change. The probe uses the same shape.
	js := fmt.Sprintf(`(function(){%s
el.focus();
if('value' in el){el.value=%s;}else{el.textContent=%s;}
el.dispatchEvent(new Event('input',{bubbles:true}));
el.dispatchEvent(new Event('change',{bubbles:true}));
%s
return JSON.stringify({url:location.href,title:document.title,value:('value' in el?el.value:el.textContent)});})()`,
		resolveRefJS(idx), jsString(in.Text), jsString(in.Text), submitJS(in.Submit))

	state, meta := a.pageJS(ctx, target, js)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	if out, stale := staleRef(state, in.Ref); stale {
		return out, nil
	}
	if in.Submit {
		// Submitting navigates; the value read above belongs to the old page.
		return a.stateAfterSettle(ctx, target)
	}
	return api.BrowserOutput{URL: state.URL, Title: state.Title, Value: state.Value, WindowID: int(target.windowID)}, nil
}

func (a *Adapter) browserPress(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	key := jsString(in.Key)
	js := fmt.Sprintf(`(function(){
var el=document.activeElement||document.body;
var opts={key:%s,bubbles:true,cancelable:true};
el.dispatchEvent(new KeyboardEvent('keydown',opts));
el.dispatchEvent(new KeyboardEvent('keypress',opts));
el.dispatchEvent(new KeyboardEvent('keyup',opts));
if(%s==='Enter'&&el.form){el.form.requestSubmit?el.form.requestSubmit():el.form.submit();}
return JSON.stringify({url:location.href,title:document.title});})()`, key, key)

	if _, meta := a.pageJS(ctx, target, js); meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	return a.stateAfterSettle(ctx, target)
}

func (a *Adapter) browserScroll(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	amount := defaultScrollAmount
	if in.Amount != nil && *in.Amount > 0 {
		amount = *in.Amount
	}
	dx, dy := 0, 0
	switch in.Direction {
	case api.DirectionUp:
		dy = -amount
	case api.DirectionDown:
		dy = amount
	case api.DirectionLeft:
		dx = -amount
	case api.DirectionRight:
		dx = amount
	}
	js := fmt.Sprintf(`(function(){window.scrollBy(%d,%d);return JSON.stringify({url:location.href,title:document.title,scroll_x:Math.round(window.scrollX),scroll_y:Math.round(window.scrollY)});})()`, dx, dy)

	state, meta := a.pageJS(ctx, target, js)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	x, y := state.ScrollX, state.ScrollY
	return api.BrowserOutput{URL: state.URL, Title: state.Title, ScrollX: &x, ScrollY: &y, WindowID: int(target.windowID)}, nil
}

func (a *Adapter) browserBack(ctx context.Context, target pageTarget) (api.BrowserOutput, error) {
	js := `(function(){history.back();return JSON.stringify({url:location.href,title:document.title});})()`
	if _, meta := a.pageJS(ctx, target, js); meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	return a.stateAfterSettle(ctx, target)
}

func (a *Adapter) browserText(ctx context.Context, target pageTarget, in api.BrowserInput) (api.BrowserOutput, error) {
	var js string
	if in.Ref == "" {
		js = `(function(){var t=(document.body?document.body.innerText:'')||'';return JSON.stringify({url:location.href,title:document.title,text:t.slice(0,200000)});})()`
	} else {
		idx, err := refIndex(in.Ref)
		if err != nil {
			return browserErr(api.CodeInvalidArgument, err.Error(), false), nil
		}
		js = fmt.Sprintf(`(function(){%s var t=(el.innerText||el.textContent||'');return JSON.stringify({url:location.href,title:document.title,text:t.slice(0,200000)});})()`,
			resolveRefJS(idx))
	}

	state, meta := a.pageJS(ctx, target, js)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	if out, stale := staleRef(state, in.Ref); stale {
		return out, nil
	}
	return api.BrowserOutput{URL: state.URL, Title: state.Title, Text: state.Text, WindowID: int(target.windowID)}, nil
}

// stateAfterSettle waits for a navigation to land and reports the page it
// landed on.
func (a *Adapter) stateAfterSettle(ctx context.Context, target pageTarget) (api.BrowserOutput, error) {
	select {
	case <-time.After(settleDelay):
	case <-ctx.Done():
		return browserErr(api.CodeDeadlineExceeded, "browser: ctx expired while the page settled: "+ctx.Err().Error(), false), nil
	}
	state, meta := a.pageJS(ctx, target,
		`(function(){return JSON.stringify({url:location.href,title:document.title,scroll_x:Math.round(window.scrollX),scroll_y:Math.round(window.scrollY)});})()`)
	if meta.Code != "" {
		return api.BrowserOutput{ResultMeta: meta}, nil
	}
	return api.BrowserOutput{URL: state.URL, Title: state.Title, WindowID: int(target.windowID)}, nil
}

// ---------------------------------------------------------------------------
// JavaScript construction
// ---------------------------------------------------------------------------

// jsString renders s as a JavaScript string literal. Every caller-supplied
// value reaches the page through this: a URL, typed text, or key name is
// arbitrary input, and concatenating it raw would break on a quote and would
// let a caller inject script into the program we are building.
func jsString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail; fall back to an empty literal
		// rather than emitting something unquoted.
		return `""`
	}
	return string(encoded)
}

// resolveRefJS emits the prologue every ref action shares: look the ref up in
// the array the last snapshot left behind, and bail out if it no longer
// resolves. A navigation replaces the document (and with it the array), and a
// re-render can detach the node while the array still holds it -- both must be
// reported, never silently retargeted.
func resolveRefJS(idx int) string {
	return fmt.Sprintf(`var refs=window.__hadronRefs||[];var el=refs[%d];
if(!el||!document.contains(el)){return JSON.stringify({error:'stale_ref'});}`, idx)
}

func submitJS(submit bool) string {
	if !submit {
		return ""
	}
	return `if(el.form){el.form.requestSubmit?el.form.requestSubmit():el.form.submit();}`
}

// snapshotJS builds the snapshot program: collect the page's interactive
// elements with their roles, accessible names and bounds, and leave the nodes
// in window.__hadronRefs for later ref actions.
//
// The nodes are kept in a JavaScript array rather than stamped onto the DOM as
// data-* attributes: attributes are visible to the page's own selectors and
// CSS, so marking them up could change the very page we are inspecting.
func snapshotJS(limit int) string {
	return fmt.Sprintf(`(function(){
var sel='a[href],button,input,select,textarea,summary,[role=button],[role=link],[role=textbox],[role=checkbox],[role=tab],[role=menuitem],[onclick],[tabindex]';
var nodes=[].slice.call(document.querySelectorAll(sel));
var out=[],refs=[],truncated=false;
for(var i=0;i<nodes.length;i++){
  if(out.length>=%d){truncated=true;break;}
  var el=nodes[i];
  var r=el.getBoundingClientRect();
  if(r.width<=0||r.height<=0){continue;}
  var st=window.getComputedStyle(el);
  if(st.visibility==='hidden'||st.display==='none'||st.opacity==='0'){continue;}
  if(el.disabled){continue;}
  var name=(el.getAttribute('aria-label')||el.innerText||el.getAttribute('title')||el.getAttribute('placeholder')||el.getAttribute('alt')||el.value||'');
  name=String(name).replace(/\s+/g,' ').trim().slice(0,200);
  refs.push(el);
  out.push({ref:'e'+refs.length,
            role:(el.getAttribute('role')||el.tagName.toLowerCase()),
            name:name,
            value:String(el.value||'').slice(0,200),
            x:Math.round(r.left),y:Math.round(r.top),
            width:Math.round(r.width),height:Math.round(r.height)});
}
window.__hadronRefs=refs;
return JSON.stringify({url:location.href,title:document.title,elements:out,truncated:truncated});})()`, limit)
}

// refIndex turns a public ref ("e12") into its index in window.__hadronRefs.
func refIndex(ref string) (int, error) {
	if !strings.HasPrefix(ref, "e") {
		return 0, fmt.Errorf("invalid ref %q: refs come from a snapshot and look like \"e1\"", ref)
	}
	n, err := strconv.Atoi(ref[1:])
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid ref %q: refs come from a snapshot and look like \"e1\"", ref)
	}
	return n - 1, nil
}

// staleRef converts the script's own stale-ref report into the public error.
// This is the property that justifies ref targeting over coordinates: a ref
// that outlived its page fails loudly instead of acting on whatever now
// occupies that position.
func staleRef(state pageState, ref string) (api.BrowserOutput, bool) {
	if state.Error != "stale_ref" {
		return api.BrowserOutput{}, false
	}
	return browserErr(api.CodeInvalidArgument,
		fmt.Sprintf("browser: ref %q no longer resolves (the page navigated or re-rendered); take a new snapshot", ref),
		false), true
}

// ---------------------------------------------------------------------------
// page transport
// ---------------------------------------------------------------------------

// pageJS runs one script in the page and decodes the JSON object it returns.
func (a *Adapter) pageJS(ctx context.Context, target pageTarget, js string) (pageState, api.ResultMeta) {
	result, meta := a.invokeAs(ctx, toolPage, map[string]any{
		"action":     "execute_javascript",
		"pid":        target.pid,
		"window_id":  target.windowID,
		"cdp_port":   browserCDPPort,
		"javascript": js,
	}, false, "browser")
	if meta.Code != "" {
		return pageState{}, meta
	}

	var state pageState
	if err := unwrapJSON(contentText(result), &state); err != nil {
		return pageState{}, metaFor(api.CodeInternal,
			"browser: decoding the page's reply: "+err.Error(), false)
	}
	return state, api.ResultMeta{}
}

// unwrapJSON decodes the object a script returned.
//
// How execute_javascript presents that value is a driver detail this contract
// should not depend on, and in practice it is layered. Observed against the
// real Linux backend, a reply looks like:
//
//	cdp.runtime.evaluate.user_gesture: "{\"url\":\"https://kairos.io/\", ...}"
//
// -- a label naming the CDP path it took, then the script's value as an
// escaped JSON string. Other replies arrive as a bare object, or as a string
// wrapped more than once. So peel: strip a leading label, unwrap a JSON string
// layer, and try to decode, until something decodes or there is nothing left
// to peel.
func unwrapJSON(text string, out any) error {
	payload := strings.TrimSpace(text)
	if payload == "" {
		return fmt.Errorf("empty reply")
	}
	for range 6 {
		if payload == "" {
			break
		}
		if err := json.Unmarshal([]byte(payload), out); err == nil {
			return nil
		}
		// A JSON string layer: unwrap it and look again.
		var inner string
		if err := json.Unmarshal([]byte(payload), &inner); err == nil {
			payload = strings.TrimSpace(inner)
			continue
		}
		// A driver label prefixing the value.
		if rest, ok := stripLabel(payload); ok {
			payload = rest
			continue
		}
		break
	}
	return fmt.Errorf("unrecognized reply %.160q", text)
}

// stripLabel removes a leading `some.label: ` prefix, as the page backend puts
// in front of a script's value. The label is deliberately narrow -- letters,
// digits, dot, underscore, dash -- so it cannot match the start of JSON: an
// object begins with '{' and a string with '"', neither of which is a legal
// label character.
func stripLabel(payload string) (string, bool) {
	colon := strings.IndexByte(payload, ':')
	if colon <= 0 || colon == len(payload)-1 {
		return "", false
	}
	for _, r := range payload[:colon] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return "", false
		}
	}
	return strings.TrimSpace(payload[colon+1:]), true
}

func browserErr(code api.ErrorCode, message string, retryable bool) api.BrowserOutput {
	return api.BrowserOutput{ResultMeta: metaFor(code, message, retryable)}
}
