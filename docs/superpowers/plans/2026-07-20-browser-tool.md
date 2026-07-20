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

- [ ] Add `ToolBrowser`, the `BrowserAction` closed set, `BrowserInput`,
      `BrowserOutput`, and `BrowserElement` to `internal/api`.
- [ ] `Validate` enforces per-action fields: `url` for navigate, `ref` for
      click/type, `text` for type, `key` for press, and rejects fields
      irrelevant to the action (the convention `ComputerUseInput` already uses).
- [ ] Register `BrowserAction` in the enum registry so the action set is
      published as a JSON Schema enum, not merely enforced.
- [ ] Add `CodeBrowserUnavailable` = `BROWSER_UNAVAILABLE`, distinct from
      `SESSION_UNAVAILABLE` (the desktop session is up; no browser with a
      reachable CDP endpoint is).
- [ ] `ToolNames` returns eight; `RegisterAll` registers the tool.
- [ ] Tests pin the published enum, the validation matrix, and the new code.

### BT-2: Cua adapter

- [ ] `Adapter.Browser` in `internal/cua/browser.go`, composing every action as
      a single self-contained JavaScript program over `page`
      `execute_javascript` -- the only page action implemented on the Linux
      backend.
- [ ] Resolve the target window automatically via `list_windows` so callers
      never pass a pid; optional `window_id` disambiguates when more than one
      browser is open, and an ambiguous auto-resolve is an explicit
      `INVALID_ARGUMENT`, not a silent pick.
- [ ] `snapshot` records nodes in `window.__hadronRefs` (not `data-*`
      attributes, which the page's own selectors and CSS can observe) and
      returns ref/role/name/bounds.
- [ ] Ref actions re-check the ref against the live array and return
      `INVALID_ARGUMENT` naming the ref when it is stale or absent.
- [ ] Tolerant unwrapping of `execute_javascript`'s wrapped/escaped return
      value, since the exact wrapping is a driver detail.
- [ ] Table tests with a fake `Caller` covering every action, the ambiguity
      error, and the stale-ref error.

### BT-3: session broker

- [ ] `Broker.Browser` guarded like `ComputerUse` (not behind the shell/file
      semaphore; the adapter's single seat already serializes).
- [ ] Dispatch `api.ToolBrowser` in `Broker.Call`.

### BT-4: gateway

- [ ] `registerTools` registers `browser`.
- [ ] `Gateway.pick` sends `browser` to the session broker for both classes.
- [ ] Contract tests: eight tools advertised; the enum reaches `tools/list`;
      an admin bearer's `browser` call lands on the session broker, not root.

### BT-5: smoke suite

- [ ] `checkSevenTools` becomes `checkEightTools`; exercise `browser` in the
      public-contract suite.

### BT-6: documentation sweep

- [ ] Update every "seven" that is now wrong: design doc goals #4, the contract
      table, gate 2, gate 3, acceptance #4; the roadmap; the phase-2 plan; and
      the package/tool comments in `session/service.go`, `smoke/descriptor.go`,
      `cmd/mcp-smoke/main.go`.

### BT-7: end-to-end proof

- [ ] Replace the recording demo's coordinate click on "Docs" with `snapshot` +
      `click(ref)`, and re-record. This is the gate that proves the tool solves
      the problem that motivated it.
