# Browser Tool Amendment

**Date:** 2026-07-20

**Status:** Proposed. The shape is approved (one action-namespaced `browser`
tool rather than a Hermes-style `browser_*` family); the details below are
pending review and an implementation-plan amendment.

**Scope:** Add an eighth public MCP tool, `browser`, that drives web content
through the page's own DOM instead of through desktop pixels. This amends the
"exactly seven public tools" constraint in
`2026-07-14-cua-agent-appliance-design.md`.

## Problem

The appliance can drive web content today only as pixels: capture the screen,
find a target by eye, click an (x, y). That is what a computer-use agent does,
and it works — the 2026-07-20 end-to-end recording installs Chromium from
Flathub, opens `kairos.io`, and clicks through to the documentation entirely
through `computer_use`. But it is brittle in a specific, structural way, and
the fallback it depends on is not sound as a contract:

- **The desktop accessibility path does not cover web content.** Flatpak
  Chromium publishes no AT-SPI DOM tree. `computer_use accessibility` scoped to
  a Chromium window returns a single node — the window itself — so there is
  nothing to locate a link by name with. Phase 1 Task 5 already established
  this and approved CDP as Cua's actual browser path
  (`test/agent/cmd/cua-compat-probe/main.go`).
- **Coordinates encode the wrong things.** The recording only found the "Docs"
  link after the browser was forced fullscreen, because `kairos.io` collapses
  its navigation behind a hamburger at tiling-window width. A coordinate that
  works is a coordinate that silently depends on window geometry, viewport
  width, responsive breakpoints, font metrics, scroll offset, and banner
  dismissal. None of that is stable across a release, and a stale coordinate
  fails by clicking *something else* rather than by erroring.

A caller that wants "click the Docs link" should be able to say so.

## Decision

Add one public tool named `browser`, with a closed `action` set. One tool
rather than a `browser_*` family keeps the public surface small and reviewable:
the contract grows by a single name and a single enum, not by nine names.

| Action | Arguments | Result |
|---|---|---|
| `navigate` | `url` | Final URL and document title after load settles. |
| `snapshot` | `max_elements` | Interactive elements, each with a `ref`, role, accessible name, and bounds. |
| `click` | `ref` | Post-click URL and title. |
| `type` | `ref`, `text`, `submit` | Resulting field value. |
| `press` | `key` | Key delivered to the focused element. |
| `scroll` | `direction`, `amount` | Resulting scroll offset. |
| `back` | — | URL and title after navigation. |
| `text` | `ref` (optional) | Visible text of the element, or of the document. |

`snapshot` is the tool's centre of gravity: it returns the page's *own*
accessibility information — the DOM's roles and accessible names — which is
what a desktop AT-SPI tree cannot supply for a browser. Every targeting action
takes a `ref` from a snapshot, never a coordinate.

### Ref identity

`snapshot` assigns each returned element a stable `ref` (`e1`, `e2`, …) and
records the mapping in a page-scoped JavaScript array
(`window.__hadronRefs`) rather than by mutating the DOM. Stamping
`data-*` attributes onto nodes would be visible to the page's own selectors and
CSS; an array touches one global and leaves the document untouched.

Refs are valid until the page navigates or the array is replaced by the next
snapshot. Any ref action against a stale or missing ref returns
`INVALID_ARGUMENT` naming the ref, so a caller that skipped a re-snapshot after
navigation gets an explicit error rather than a click on whatever now occupies
that position. **This is the property coordinates cannot offer** and is the
main reason to add the tool at all.

### Backing implementation

The broker implements every action on top of Cua's `page` tool over CDP, which
the compatibility probe already exercises against this exact image.

One constraint governs the design: **on the Linux page backend only
`execute_javascript` is implemented.** `query_dom` compound selectors,
`click_element`, `insert_text`, and `type_keystrokes` all report "not
implemented" (`cua-compat-probe/main.go`). So `browser` does not map its
actions onto Cua page actions of the same name — it composes each one as a
single self-contained JavaScript program evaluated in the page, which performs
the action *and* reads back the resulting state in the same round trip. The
probe already uses precisely this technique for its click and type checks.

This means the tool depends on one Cua capability (`execute_javascript`), not
on eight, which is the narrowest possible coupling to a component whose Linux
backend is visibly incomplete.

## Security

The browser is reached over the Chrome DevTools Protocol, which is full control
of the browser: its cookies, its authenticated sessions, its stored
credentials, and arbitrary script execution in any origin it has loaded. That
makes the port itself the thing to constrain.

- **The CDP port binds loopback only.** Chromium's `--remote-debugging-port`
  defaults to `127.0.0.1`; the launcher must never pass
  `--remote-debugging-address`, and the appliance must never forward or proxy
  the port. It is not a LAN-reachable interface at any point.
- **The gateway exposes no raw CDP passthrough in v1.** Hermes gates its
  equivalent (`browser_cdp`) separately for the same reason: a passthrough is
  an unbounded contract and an arbitrary-code channel, whereas the eight
  actions above are a fixed, auditable surface. This is a deliberate non-goal,
  not an omission.
- **`browser` is a session-broker tool only, never a root-helper tool.** Like
  `computer_use`, it is routed to the unprivileged broker for *both* credential
  classes: an administrator bearer does not get a root-owned browser. The
  browser always runs as `agent`, in the agent's own session.
- **No new local privilege.** The `agent` user can already start a browser and
  drive it; CDP on loopback grants that user nothing it did not have. The
  boundary this preserves is the one that matters — the OS user and the VM.
- Emergency pause must stop `browser` calls exactly as it stops every other
  tool, and the paused state must not leave a live CDP channel usable.

## Contract impact

The "exactly seven tools" statement is load-bearing in more places than the
design document, and all of them must move together or the gates will
contradict each other:

| Location | Change |
|---|---|
| `specs/2026-07-14-...-design.md` goals #4, §Public MCP contract, gate 2, gate 3, acceptance #4 | seven → eight; add `browser` to the contract table |
| `agent/internal/api/schema.go` | `ToolBrowser`, `BrowserInput`/`BrowserOutput`, `BrowserActions` enum, `RegisterAll` |
| `agent/internal/gateway/server.go` | route `browser` to the session broker for both classes |
| `agent/internal/session/service.go` | implement the eight actions over Cua `page` |
| `agent/internal/smoke/{descriptor,checks}.go` | `checkSevenTools` → eight; exercise `browser` |
| `internal/gateway/contract_test.go` | `TestExactSevenToolsRegistered` → eight |
| `plans/...-roadmap.md`, `plans/...-phase-2-mcp-services.md` | seven → eight |

`browser` inherits the existing error codes and adds one:
`BROWSER_UNAVAILABLE`, for when no browser with a reachable CDP endpoint is
running — distinct from `SESSION_UNAVAILABLE`, which means the desktop session
itself is down.

## Verification

- Contract tests: the server advertises exactly eight tools; `browser.action`
  is published as a JSON Schema enum (per the 2026-07-20 enum change, closed
  sets are advertised, not merely enforced).
- A ref obtained from `snapshot` and used after a `navigate` returns
  `INVALID_ARGUMENT` rather than acting on the wrong element.
- The ordinary and administrator bearers both reach the browser as `agent`.
- The CDP port is not reachable from outside the guest; a gate asserts this
  from the host side of the fixture.
- Gate 3 (UI E2E) replaces the recording's coordinate click on "Docs" with
  `snapshot` + `click(ref)`, which is the end-to-end proof that the tool solves
  the problem that motivated it.

## Alternatives rejected

**Desktop AT-SPI for web content.** Does not exist for flatpak Chromium; a
window-scoped accessibility query returns one node. Established in Phase 1 Task
5 and re-confirmed by the 2026-07-20 recording.

**Coordinates only (status quo).** Works, and stays available through
`computer_use` for anything that is not a browser. Rejected as the *only* web
path because a stale coordinate misclicks silently instead of failing.

**A Hermes-style `browser_*` family.** Closest emulation, and the action names
map one-to-one. Rejected because it grows the frozen public surface from seven
names to roughly fifteen for no capability that an action enum does not also
provide.

**Folding the actions into `computer_use`.** Keeps the count at seven, but
overloads one tool with two different targeting models — pixels and refs —
whose arguments are mutually meaningless. The validation rules would have to
reject most field combinations, which is what a separate tool expresses
directly.

## Addendum (2026-07-20): limits found by stress-testing

Driving DOOM in a browser (js-dos) exercised the boundary between `browser` and
`computer_use` harder than the docs gate does. Four things came out of it; three
are fixed, one is a standing limitation.

### Fixed: a scripted click is not a user gesture

`browser click` originally called `el.click()` in the page. That is not user
activation, so the Fullscreen API, clipboard access and autoplay all refuse it
-- and refuse it *silently*: clicking a page's "Fullscreen" control returned
success and nothing happened.

`click` now scrolls the element into view, converts its box to screen
coordinates, and dispatches a REAL pointer event, falling back to `el.click()`
only when that conversion is unavailable. The result reports `click_method`
(`pointer` or `script`) so a caller can tell a trusted click from an untrusted
one instead of watching a successful call do nothing.

### Fixed: viewport coordinates are not screen coordinates

`getBoundingClientRect` is viewport-relative; `computer_use` clicks in screen
coordinates. They differ by the window position plus the browser chrome -- 156px
on a stock window here. The spec previously offered element bounds so a caller
could "fall back to computer_use", which was misleading: doing that literally
put a click 158px above its target.

Elements now carry `screen_x`/`screen_y` alongside `x`/`y`, derived from the
window box (`get_window_state`, whose frames are screen-relative) and the
viewport size reported by the same script. They are omitted, rather than
defaulted, when the geometry cannot be read.

### Fixed: role-less clickables were invisible

The selector required `a[href]`. js-dos's "Click to start" is an anchor with no
href wired up in JavaScript, so the one control that starts the game did not
appear in any snapshot -- all 17 elements were ordinary links. The selector now
takes bare `a` and `canvas`, plus a bounded second pass over elements whose
computed `cursor` is `pointer`.

### Standing limitation: keys cannot be held

`computer_use key` delivers an atomic tap. There is no key-down/key-up pair and
no hold duration, so hold-to-act interfaces -- real-time games, press-and-hold
controls, click-drag-with-modifier gestures -- cannot be driven through the
public surface. In DOOM this is visible as a marine who turns a sliver per
keypress and never fires.

This is not a gap we can close locally. Verified against the shipped image:

- `cua-driver` exposes no `key_down`/`key_up`; `press_key` is atomic, and its
  `duration_ms` strings belong to mouse glide/dwell, not key hold.
- the image carries no `xdotool`, `xte`, `ydotool` or `wtype`, so the
  `terminal` tool offers no escape hatch either.

Two upgrade paths exist, neither free:

1. **Upstream `key_down`/`key_up` in cua-driver**, then expose `duration_ms` on
   `computer_use key`. Correct, but gated on a component we do not release.
2. **Ship a local XTEST helper** and implement the hold in the session broker.
   Self-contained, but it injects input outside the Cua adapter, and so
   **bypasses the single-seat serialization** that keeps a snapshot from
   interleaving with a click. That invariant is deliberate.

Until a real use case demands it, this stays a documented non-goal: the
appliance automates applications, and hold-to-act is a game and CAD need.
