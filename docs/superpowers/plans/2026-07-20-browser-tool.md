# Plan amendment: the `browser` tool

**Date:** 2026-07-20

**Amends:** `2026-07-14-cua-agent-appliance-roadmap.md` and
`2026-07-14-cua-agent-phase-2-mcp-services.md`, both of which state a
seven-tool contract.

**Design:** `specs/2026-07-20-browser-tool-amendment.md`

**Goal:** Add an eighth public MCP tool, `browser`, that targets web content by
ref from the page's own accessibility information instead of by screen
coordinate, implemented over Cua's `page` tool using `execute_javascript`
alone.

## Invariants this must not break

- `browser` routes to the **unprivileged session broker for both credential
  classes**. An administrator bearer must not get a root-owned browser. This is
  the same exception `computer_use` already has in `Gateway.pick`.
- The CDP port is **loopback-only** and never forwarded, proxied, or made
  configurable to bind elsewhere.
- No raw CDP passthrough action ships in v1.
- Emergency pause must reject `browser` exactly as it rejects every other tool.
- A ref that outlives its page must **error**, never act on whatever now
  occupies that position.

## Tasks

### BT-1: api contract

- [x] Add `ToolBrowser`, the `BrowserAction` closed set, `BrowserInput`,
      `BrowserOutput`, and `BrowserElement` to `internal/api`.
- [x] `Validate` enforces per-action fields: `url` for navigate, `ref` for
      click/type, `text` for type, `key` for press, and rejects fields
      irrelevant to the action (the convention `ComputerUseInput` already uses).
- [x] Register `BrowserAction` in the enum registry so the action set is
      published as a JSON Schema enum, not merely enforced.
- [x] Add `CodeBrowserUnavailable` = `BROWSER_UNAVAILABLE`, distinct from
      `SESSION_UNAVAILABLE` (the desktop session is up; no browser with a
      reachable CDP endpoint is).
- [x] `ToolNames` returns eight; `RegisterAll` registers the tool.
- [x] Tests pin the published enum, the validation matrix, and the new code.

### BT-2: Cua adapter

- [x] `Adapter.Browser` in `internal/cua/browser.go`, composing every action as
      a single self-contained JavaScript program over `page`
      `execute_javascript` -- the only page action implemented on the Linux
      backend.
- [x] Resolve the target window automatically via `list_windows` so callers
      never pass a pid; optional `window_id` disambiguates when more than one
      browser is open, and an ambiguous auto-resolve is an explicit
      `INVALID_ARGUMENT`, not a silent pick.
- [x] `snapshot` records nodes in `window.__hadronRefs` (not `data-*`
      attributes, which the page's own selectors and CSS can observe) and
      returns ref/role/name/bounds.
- [x] Ref actions re-check the ref against the live array and return
      `INVALID_ARGUMENT` naming the ref when it is stale or absent.
- [x] Tolerant unwrapping of `execute_javascript`'s wrapped/escaped return
      value, since the exact wrapping is a driver detail.
- [x] Table tests with a fake `Caller` covering every action, the ambiguity
      error, and the stale-ref error.

### BT-3: session broker

- [x] `Broker.Browser` guarded like `ComputerUse` (not behind the shell/file
      semaphore; the adapter's single seat already serializes).
- [x] Dispatch `api.ToolBrowser` in `Broker.Call`.

### BT-4: gateway

- [x] `registerTools` registers `browser`.
- [x] `Gateway.pick` sends `browser` to the session broker for both classes.
- [x] Contract tests: eight tools advertised; the enum reaches `tools/list`;
      an admin bearer's `browser` call lands on the session broker, not root.

### BT-5: smoke suite

- [x] `checkSevenTools` becomes `checkEightTools`; exercise `browser` in the
      public-contract suite.

### BT-6: documentation sweep

- [x] Update every "seven" that is now wrong: design doc goals #4, the contract
      table, gate 2, gate 3, acceptance #4; the roadmap; the phase-2 plan; and
      the package/tool comments in `session/service.go`, `smoke/descriptor.go`,
      `cmd/mcp-smoke/main.go`.

### BT-7: end-to-end proof

- [x] Replace the recording demo's coordinate click on "Docs" with `snapshot` +
      `click(ref)`, and re-record. This is the gate that proves the tool solves
      the problem that motivated it.

## Outcome (2026-07-20)

All tasks complete. The live run against the real Cua page backend:

```
browser:snapshot  -> 73 interactive elements from https://kairos.io/
found 'Docs' ref=e6 role=a          <- located by NAME, not by pixel
browser:click(e6) -> https://kairos.io/docs/v4.1.2/ "Documentation | Kairos"
browser:text      -> "...Kairos\nQuick StartDocsBlogCommunity\n..."
```

Recording: `build/recording/agent-e2e-browser-tool.mp4` (65s).

Three bugs the process caught, in the order they were found:

1. A malformed ref reported `INTERNAL`, because parsing ran after window
   resolution. Found by a unit test; fixed by parsing before any dispatch.
2. `isBrowserWindow` matched the window title, so the shell running
   `flatpak install org.chromium.Chromium` looked like a second browser and
   made auto-resolution ambiguous. Found by reading the code against what the
   demo actually does; fixed by matching `app_name`.
3. The page backend prefixes its reply with a CDP path label
   (`cdp.runtime.evaluate.user_gesture: "{...}"`), which `unwrapJSON` could not
   peel. Found only by the LIVE run -- no fake could have predicted it -- and
   the test now pins the exact captured string.

Item 3 is the argument for keeping this gate: the unit tests were green and the
tool was still completely non-functional against the real backend.
