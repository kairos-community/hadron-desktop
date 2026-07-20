# Cua Agent Phase 2: MCP Services and Privilege Boundary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build one static Hadron agent binary that exposes exactly eight authenticated MCP tools while preserving the unprivileged desktop boundary and independently validating optional administrator access.

**Architecture:** One binary has `gateway`, `session`, `root-helper`, `control`, `status`, and later `provision` subcommands. The HTTPS gateway is the only listener. It validates bearer credentials and proxies fixed-schema calls over permissioned Unix HTTP sockets. The agent-owned session broker implements shell, PTY process, file, and serialized Cua operations. The optional root helper implements only OS tools and revalidates the raw admin bearer. Streamable HTTP is stateless so emergency pause can cancel all active work without hidden SDK session state.

**Tech Stack:** Go 1.25, MCP Go SDK 1.6.1, `creack/pty` 1.1.24, Go standard library HTTP/TLS/process/filesystem APIs, Unix sockets, Cua MCP stdio.

## Global Constraints

- The public server advertises exactly `computer_use`, `terminal`, `process`, `read_file`, `search_files`, `write_file`, and `patch`.
- Do not expose Cua's raw tools, REST, WebSocket, stdio, or Unix socket publicly.
- The gateway never executes a command, opens an arbitrary path, starts Cua, runs as root, or accesses Docker.
- An ordinary bearer always routes OS tools to the agent broker.
- An admin `computer_use` call still routes to the agent broker; only the other six tools may route to root.
- The root helper authenticates the raw `hdn_a_` bearer against its own digest copy.
- Bearer values, commands, typed text, file contents, patches, and screenshots never enter audit logs.
- Tool errors are MCP tool results with typed codes; malformed MCP traffic remains a protocol error.
- Use bounded bodies, output, execution time, process count, and concurrency everywhere.

---

## File Structure

**Created under `agent/`:**

- `cmd/hadron-agent/main.go`, `main_test.go` — subcommand dispatch and exit behavior.
- `internal/api/schema.go`, `schema_test.go` — eight stable public input/output types and error codes.
- `internal/auth/token.go`, `token_test.go` — token generation, digest verification, rotation, credential context.
- `internal/files/service.go`, `service_test.go` — bounded read/search/write/patch.
- `internal/process/manager.go`, `cgroup.go`, and tests — shell, PTY processes, delegated cgroups, ring buffers, cleanup.
- `internal/cua/adapter.go`, `adapter_test.go` — unified computer-use-to-Cua mapping.
- `internal/rpc/protocol.go`, `client.go`, `server.go`, and tests — fixed internal Unix protocol.
- `internal/session/service.go`, `service_test.go` — unprivileged broker.
- `internal/roothelper/service.go`, `service_test.go` — independently authenticated privileged broker.
- `internal/control/controller.go`, `controller_test.go` — pause generation, cancellation, active state.
- `internal/gateway/server.go`, `server_test.go` — MCP/HTTPS/auth/health/audit endpoint.
- `internal/gateway/contract_test.go` — end-to-end in-process MCP contract.
- `internal/testutil/` — fake Cua, fake executors, bearer RoundTripper, and Unix test servers.

**Modified:** `agent/go.mod` and `agent/go.sum`.

## Task 1: Freeze public schemas and error semantics

- [ ] Write failing tests that assert the sorted tool names equal:

```text
computer_use
patch
process
read_file
search_files
terminal
write_file
```

- [ ] Define `computer_use.action` as `capture`, `accessibility`, `click`, `double_click`, `drag`, `scroll`, `type`, `key`, `wait`, `list_applications`, or `focus_application`. Its single input type contains optional `scope`, `pid`, `window_id`, `element_index`, `x`, `y`, `from_x`, `from_y`, `to_x`, `to_y`, `button`, `direction`, `amount`, `text`, `key`, `modifiers`, `duration_ms`, `query`, `max_elements`, and `max_depth` fields. Per-action validation rejects irrelevant or missing combinations.

- [ ] Freeze the six OS schemas:

| Tool | Required contract |
|---|---|
| `terminal` | `command`; optional `cwd`, `env`, `timeout_ms`, `max_output_bytes` |
| `process` | `action=start,poll,write,terminate`; process ID plus start/write options, optional `pty` |
| `read_file` | `path`; optional `offset`, `limit`, `encoding=utf8,base64`, `metadata_only` |
| `search_files` | `path`, `query`; `mode=name,content`, regex flag, max results/file bytes |
| `write_file` | `path`, `content`; `encoding`, optional mode and parent creation |
| `patch` | `files[]`, each with `path` and exact `replacements[] {old,new,all}` |

- [ ] Define stable errors `INVALID_ARGUMENT`, `UNAUTHENTICATED`, `FORBIDDEN`, `SESSION_UNAVAILABLE`, `NOT_FOUND`, `ALREADY_EXISTS`, `RESOURCE_EXHAUSTED`, `DEADLINE_EXCEEDED`, `OUTPUT_TRUNCATED`, `PAUSED`, and `INTERNAL`. Each result includes `code`, safe `message`, `retryable`, and execution `identity` when applicable.

- [ ] Use typed `mcp.AddTool` registration and inferred JSON schemas. Run `go test ./internal/api -count=1`, then commit with `git commit -m "feat(agent): define the seven-tool MCP contract"`.

## Task 2: Implement bearer generation, verification, and rotation test-first

- [ ] Write tests for 32 random bytes, `hdn_u_` and `hdn_a_` prefixes, malformed tokens, wrong class, current digest, unexpired previous digest, expired previous digest, and non-short-circuit comparison across configured digests.

- [ ] Store digests only as the literal `sha256:` prefix followed by 64 lowercase hexadecimal characters. Implement generation with `crypto/rand` and unpadded base64url. Verify by hashing the complete bearer and applying `subtle.ConstantTimeCompare` to every candidate digest before combining results.

- [ ] Return MCP `auth.TokenInfo` with scopes `hadron:user` or `hadron:admin` and `UserID` equal to the first 16 hex digest characters. Put a private credential object containing class and raw bearer in `TokenInfo.Extra["credential"]` so only the root-helper proxy can forward it. Never format this object through `Stringer`.

- [ ] Add rotation fields `current`, `previous`, and `previous_valid_until`. Reject overlap longer than 24 hours.

- [ ] Run `go test ./internal/auth -count=1` and `go test -race ./internal/auth`. Commit as `feat(agent): add scoped bearer verification and rotation`.

## Task 3: Implement bounded file tools

- [ ] Write table tests for UTF-8/base64 reads, offsets, metadata-only reads, filename/content literal search, regex search, result truncation, symlink behavior, atomic replace, mode application, missing parent behavior, duplicate patch match rejection, `all=true` replacement, and rollback when any file patch fails.

- [ ] Set defaults/maxima: read/write body 1 MiB/8 MiB; search 100/1000 results and 1 MiB per file; patch 128 replacements and 8 MiB aggregate. `filepath.WalkDir` does not follow directory symlinks. The OS identity is the access boundary; do not add a misleading path allowlist.

- [ ] `read_file` follows a final file symlink using normal OS permissions; `write_file` and `patch` reject a final symlink so an atomic rename cannot silently replace the link itself. `write_file` uses a same-directory temporary file, `fsync`, mode/chown preservation for replacement, atomic rename, and directory `fsync`. `patch` plans every exact replacement in memory first, rejects absent/non-unique `old` unless `all=true`, then writes all files; on a write failure restore already replaced files from same-directory backups before returning.

- [ ] Run `go test ./internal/files -count=1` and commit as `feat(agent): add bounded filesystem tools`.

## Task 4: Implement terminal and tracked PTY processes

- [ ] Add `github.com/creack/pty@v1.1.24`. Write tests for exit status, separate stdout/stderr in terminal mode, timeout, truncation metadata, environment/cwd, PTY echo, start/poll/write/terminate, unknown process, 16-process limit, ring-buffer cursor semantics, delegated cgroup placement/kill, child process-group fallback, and manager shutdown.

- [ ] `terminal` runs `/bin/sh -lc` with a 30-second default and 300-second maximum. Capture at most 1 MiB per stream by default and 8 MiB maximum.

- [ ] `process start` uses direct `/bin/sh -lc`, optional PTY, `Setpgid=true`, and `Pdeathsig=SIGKILL`. Track opaque 128-bit IDs rather than exposing PIDs. Keep 1 MiB output rings by default, accept monotonic read cursors, and cap each broker at 16 live processes. After spawn, create a cgroup-v2 leaf below the service's delegated cgroup, move the child PID into it, and apply `pids.max=128` plus `memory.max=1073741824` when those controllers are available. Production config requires cgroup placement; tests outside systemd may enable the process-group fallback.

- [ ] Termination sends SIGTERM to the negative process group, waits two seconds, writes `1` to the leaf's `cgroup.kill`, then uses SIGKILL on the group as a final fallback. Remove the empty leaf. `terminal`, `Manager.Close`, and pause use the same tracked path so grandchildren cannot escape cleanup.

- [ ] Run `go test -race ./internal/process -count=1` and commit as `feat(agent): add shell and tracked process tools`.

## Task 5: Adapt the unified computer tool to Cua

- [ ] Write fake-Cua mapping tests for every action, missing display, disconnect during a mutation, cancellation, foreground enforcement, response/image preservation, and single-seat serialization.

- [ ] Map actions exactly:

| Hadron action | Cua call |
|---|---|
| desktop capture | `get_desktop_state` |
| window capture | `get_window_state` |
| desktop accessibility | `get_accessibility_tree` |
| window accessibility | `get_window_state` with `include_screenshot=false` |
| click/double-click/drag/scroll/type | matching Cua tool with `delivery_mode=foreground` |
| one key / chord | `press_key` / `hotkey` with foreground delivery |
| wait | context-aware local timer, maximum 30 seconds |
| list applications | `list_windows` grouped by PID/app name |
| focus application | `bring_to_front` |

- [ ] Never replay a failed/disconnected mutation. Return `SESSION_UNAVAILABLE` when the child or display is absent and `DEADLINE_EXCEEDED` on cancellation.

- [ ] Extend `internal/cua.Client` with a reconnect loop capped at 30 seconds and a readiness callback. Serialize calls in the adapter, not separately per client reconnect.

- [ ] Run `go test -race ./internal/cua -count=1` and commit as `feat(agent): expose unified foreground computer use`.

## Task 6: Add the fixed internal Unix RPC

- [ ] Define only `POST /v1/call`, `GET /v1/health`, `POST /v1/pause`, and `POST /v1/resume`. `CallRequest` contains `request_id`, `tool`, and raw JSON `arguments`. `CallResponse` contains the serialized MCP tool result. Limit bodies to 2 MiB and reject unknown fields.

- [ ] Use separate sockets:

```text
/run/hadron-agent/session/session.sock
/run/hadron-agent/root/root.sock
/run/hadron-agent/control/control.sock
```

The RPC package accepts already-created listeners; systemd/file permissions are Phase 3's responsibility.

- [ ] For root calls only, require the original `Authorization: Bearer ...` header. Never accept a boolean admin field or a forwarded scope as authority.

- [ ] Test truncated JSON, oversized bodies, cancellation, socket disappearance, pause/resume idempotence, unsupported tool names, and header forwarding. Run `go test -race ./internal/rpc` and commit as `feat(agent): add fixed Unix broker protocol`.

## Task 7: Build the unprivileged session broker

- [ ] Compose file, process, Cua adapter, and control state through constructor injection. Register all eight tools internally, but return `SESSION_UNAVAILABLE` only for `computer_use` when Cua is down; shell/files remain live.

- [ ] On pause, atomically reject new calls, cancel active calls, close/restart the Cua child only after resume, and terminate every MCP-owned process group. Concurrent shell/file calls use a semaphore of eight; computer calls use one seat lock.

- [ ] Require `DISPLAY`, `XAUTHORITY`, and `DBUS_SESSION_BUS_ADDRESS` only when starting Cua. Start it as `cua-driver mcp --no-daemon-relaunch` with telemetry/update checks off and accessibility advertisement `all`.

- [ ] Test broker degradation/reconnect, parallel OS calls, serialized Cua calls, pause cleanup, and identity reporting. Run `go test -race ./internal/session` and commit as `feat(agent): add the unprivileged session broker`.

## Task 8: Build the independently authenticated root helper

- [ ] Construct it with its own admin digest set and its own file/process services. Reject `computer_use` and any tool name outside the six OS tools before dispatch.

- [ ] Extract the raw bearer from the internal HTTP header and call the same constant-time verifier locally. Tests must prove a user token, invalid admin token, missing token, or gateway-supplied fake scope cannot reach an executor.

- [ ] Pause cancels root commands and process groups. Identity in successful results is `uid=0,user=root,credential=admin`.

- [ ] Run `go test -race ./internal/roothelper` and commit as `feat(agent): add independently authenticated root helper`.

## Task 9: Build the stateless HTTPS MCP gateway and local emergency controller

- [ ] Write failing contract tests for exact tools, bearer 401/403 behavior, user/admin routing, admin computer routing to agent, health redaction, readiness transitions, body/concurrency limits, origin rejection, TLS requirement, loopback-only development HTTP, audit redaction, pause, and cancellation.

- [ ] Create the MCP handler with `mcp.NewStreamableHTTPHandler` using `Stateless: true` and a 60-second request maximum. Wrap it with `http.NewCrossOriginProtection().Handler` and `auth.RequireBearerToken`. Apply auth only to `/mcp`. Typed handlers read the authenticated credential from `request.Extra.TokenInfo.Extra["credential"]`; the SDK attaches bearer metadata to `CallToolRequest.Extra`, not through an application-defined global.

- [ ] `/healthz` returns only `version` and booleans `gateway`, `session`, `cua`, `paused`. `/readyz` is 200 only when gateway, session, and Cua are ready and not paused. Neither endpoint returns paths, users, tokens, process data, or arguments.

- [ ] Default listen is `0.0.0.0:7443` with TLS 1.3 minimum. `--insecure-loopback` is accepted only when the parsed address is loopback. Limit request bodies to 2 MiB, serialized tool responses to 16 MiB, concurrent MCP calls to 16, and active HTTP connections to 64.

- [ ] The controller owns a cancellable generation for active calls. `pause` swaps/cancels it, tells both brokers to pause, writes redacted runtime status, and rejects both credential classes. `resume` creates a fresh generation and resumes brokers. `control toggle` talks only to the local control socket.

- [ ] Audit with `slog`: timestamp, request ID, token ID, credential class, tool, duration, result code, timeout/truncation flags. Add a test whose secret command/text/file/token values cannot be found in captured logs.

- [ ] Run `go test -race ./internal/gateway ./internal/control` and commit as `feat(agent): add authenticated MCP gateway and emergency pause`.

## Task 10: Wire the CLI, run full contracts, and produce a static binary

- [ ] Implement subcommands `gateway`, `session`, `root-helper`, `control pause|resume|toggle`, `status`, and `version` with `flag.FlagSet`. Unknown commands/flags exit 2; runtime errors exit 1; help exits 0. Reserve `provision` for Phase 3 with a clear unsupported error until then.

- [ ] In `contract_test.go`, start fake session/root Unix servers plus an HTTPS gateway, connect with `mcp.StreamableClientTransport` and a bearer-injecting RoundTripper, list exact tools, call each tool, test both credentials, assert root revalidation, pause an active process, and verify health redaction.

- [ ] Run:

```bash
cd agent
go test ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o /tmp/hadron-agent ./cmd/hadron-agent
file /tmp/hadron-agent
! readelf -d /tmp/hadron-agent 2>/dev/null | grep -q NEEDED
/tmp/hadron-agent version
```

Expected: all tests pass, the binary is static, and version output contains the build version plus Cua revision field.

- [ ] Commit:

```bash
git add agent
git commit -m "feat(agent): complete MCP service binary"
```

Do not wire system users or services until Phase 3.
