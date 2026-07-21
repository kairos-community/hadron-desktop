# Bash Tool Amendment

**Date:** 2026-07-21

**Status:** Implemented.

**Scope:** Replace `terminal` and `process` with a single unbounded `bash` tool.
The public contract goes from eight tools to seven.

## Decision

| Before | After |
|---|---|
| `terminal` — one command, server-imposed timeout and output cap | `bash` — a shell script, **no timeout, no output cap** |
| `process` — start / poll / write / terminate a background handle | *(removed)* |

The seven public tools are now `bash`, `browser`, `computer_use`, `patch`,
`read_file`, `search_files`, `write_file`.

## Why

`process` existed to work around `terminal`'s limits: a Flathub install takes
minutes, `terminal` was capped, so anything long had to be started as a handle
and polled. That bought a stateful surface — four actions, a process id, PTY
handling, liveness semantics — whose only purpose was to escape a bound we chose
to impose.

It also did not work reliably. The UI gate's Chromium install failed repeatedly
with `NOT_FOUND` a few seconds after start: the handle vanished mid-install and
the exit status was lost. Three rounds of debugging never reproduced it outside
the gate. Removing the bound removes the need for the handle, and with it the
entire class of failure.

Every limit this tool could impose is a limit the agent cannot lift when it
turns out to be wrong. A caller that wants a deadline writes `timeout 60 ...`
into the script — the only place that knows what the right deadline is.

## What "unbounded" does and does not mean

**Unbounded duration.** The tool sets no timer. The gateway used to apply its
60s per-request deadline at the HTTP layer, which would have made the promise a
lie regardless of what the tool did; that deadline moved into `route()`, where
the tool name is known, so `bash` runs without one and **every other tool keeps
exactly the bound it had**.

**Unbounded output.** No stream cap and no `OUTPUT_TRUNCATED`. Note `capWriter`
treats a zero limit as unbounded now — it previously meant "capture nothing",
which would have silently returned empty output.

**NOT unbounded containment.** Each call still runs in its own cgroup leaf, and
that leaf is still torn down when the call returns. A detached grandchild — one
that called `setsid` precisely to escape its process group — still dies. This is
the kill boundary the emergency pause depends on, and it is asserted by the
`bash_cgroup_containment` check.

**NOT uncancellable.** `ctx` still applies, so an emergency pause or a shutdown
cuts a running command short. Removing the timer removed the timer, not control.

## Consequences

- The gateway's `DefaultRequestTimeout` still bounds the six other tools.
- A wedged script holds a connection and a concurrency slot for as long as it
  runs. That is the deliberate trade: the alternative is a cap that is wrong for
  somebody. `MaxConcurrentCalls` still bounds the blast radius, and the operator
  retains the emergency pause.
- `mcp-smoke` loses its `run` mode; `exec` is enough now that one call can take
  as long as it needs.
- `process_pty_cgroup_isolation` became `bash_cgroup_containment`, which asserts
  the same guarantee in a stronger shape: it checks the grandchild is dead
  AFTER the call returned, which is exactly when teardown must have run.
