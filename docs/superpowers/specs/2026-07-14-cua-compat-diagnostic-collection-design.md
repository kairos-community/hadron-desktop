# Cua Compatibility Diagnostic Collection Amendment

**Date:** 2026-07-14

**Status:** Approved design; pending implementation-plan amendment

**Scope:** Resolve the two Phase 1 Task 4 contract gaps discovered against
`hadron-agent:compat` at
`sha256:e9cbfaf60c3b9154045a44d88d3710f48f56533d46534febbebc9d095d628505`:
the test image lacks `xprop` and `xrandr`, and a root system service cannot
inherit the graphical environment created by Ly and `dbus-run-session`.

## Decision

Use a split, test-only collector and reporter.

The collector runs as the unprivileged `agent` account from i3. It therefore
inherits the real `DISPLAY`, `XAUTHORITY`, and `DBUS_SESSION_BUS_ADDRESS`
created by Ly and the existing session launcher. It records graphical
diagnostics and a redacted environment in the agent runtime directory.

The reporter remains a root system service. It never runs graphical commands
and never receives, guesses, or scrapes graphical environment variables. It
collects the user journal, stages fixed artifacts, emits the serial protocol,
and writes the tar archive to the raw artifact disk.

This preserves the intended privilege boundary: graphical inspection happens
inside the actual visible session, while only the narrow operations requiring
root—reading the full journal and writing the block device—run as root.

## Pinned diagnostic clients

Build the following official X.Org application releases in the existing
test-only native fixture builder:

| Application | Version | SHA-256 |
|---|---:|---|
| `xprop` | 1.2.8 | `d689e2adb7ef7b439f6469b51cda8a7daefc83243854c2a3b8f84d0f029d67ee` |
| `xrandr` | 1.5.4 | `2cafccb2aaf2491a4068676117a0d4f90ab307724b96fffc54cd1da953779400` |

Download only from `https://www.x.org/releases/individual/app/`, verify each
digest before extraction, build against the headers and libraries copied from
the exact `BASE_IMAGE`, and copy only the two resulting binaries into the
compatibility test image. Reject the build if either binary is absent or has
an unresolved dynamic link.

Neither binary enters `hadron-agent:compat`, `Dockerfile.agent`, or either
ordinary desktop image.

## Runtime components

### Session collector

Create the executable test-only helper:

```text
/usr/local/libexec/cua-compat-collect
```

Start it from `99-cua-compat.conf` after starting the GTK fixture, Chromium
fixture, and probe. i3 starts commands asynchronously, so the collector waits
for the probe completion contract rather than relying on textual command
order.

The collector must:

1. Run as UID 1000 and reject any other UID.
2. Require nonempty inherited `DISPLAY`, `XAUTHORITY`, and
   `DBUS_SESSION_BUS_ADDRESS`; require the Xauthority path to be readable.
3. Use `/run/user/1000/hadron-cua-compat` with mode `0700` and write diagnostic
   files atomically.
4. Atomically write `collection.started`, then wait up to 240 seconds for both
   `result.json` and `result.status`.
5. Run, with telemetry and update checks explicitly disabled:
   - `cua-driver doctor --json`
   - `xprop -root`
   - `xrandr`
6. Record stdout, stderr, and the exit status of every diagnostic without
   writing those diagnostics to the serial console.
7. Write `environment.txt`, replacing the value of every environment key
   whose name contains `TOKEN`, case-insensitively, with `<redacted>`.
8. Atomically write `collection.status` as `PASS` only when the inherited
   environment is valid and all three diagnostic commands complete
   successfully; otherwise write `FAIL` after recording the reason.

The collector must never persist an unredacted environment, read another
process's environment, construct a display name, launch XLibre, or open the
artifact block device.

### Probe completion contract

Phase 1 Task 5 owns the typed result and must atomically write:

```text
/run/user/1000/hadron-cua-compat/result.json
/run/user/1000/hadron-cua-compat/result.status
/run/user/1000/hadron-cua-compat/cua-desktop.png
```

`result.status` contains exactly `PASS` when the probe's typed `AllPassed`
method returns true and exactly `FAIL` otherwise. The JSON remains the
authoritative detailed result and is independently validated by the host
harness; the status file avoids adding a second JSON parser to the guest.

The JSON and status file, and the PNG when produced, are written through
same-directory temporary files followed by atomic rename. The status file is
written last. A failed desktop capture may omit the PNG only when the typed
result and status are both `FAIL`; the reporter records the missing artifact
without converting the failure into a timeout.

### Root reporter

Keep these test-only paths:

```text
/usr/local/libexec/cua-compat-report
/etc/systemd/system/cua-compat-report.service
```

Enable the service in `multi-user.target` and order it after
`ly@tty1.service`. It must not add `Wants=` or `Requires=` edges to Ly, or
start or restart Ly, XLibre, i3, the fixtures, or the probe.

The reporter must:

1. Emit `CUACOMPAT: BEGIN` to `/dev/ttyS0` once.
2. Wait at most 60 seconds for `collection.started`, then at most 240 seconds
   for `result.status`, then at most 30 seconds for `collection.status`.
3. Stage the result JSON, Cua PNG, GTK state, Chromium commit, collector
   outputs, and a root-collected journal restricted to UID 1000.
4. Archive user-owned inputs without dereferencing symlinks outside the
   runtime directory.
5. Record internal errors in the staged artifacts, never as unrestricted
   serial output.
6. Write a tar archive at offset zero of
   `/dev/disk/by-id/virtio-hadronagentartifacts`. A write failure changes the
   final result to `FAIL`.
7. Emit exactly one `CUACOMPAT: PASS` after the write when both status files
   contain `PASS` and staging plus artifact writing succeeded; emit exactly one
   `CUACOMPAT: FAIL` otherwise.
8. Emit `CUACOMPAT: DONE` immediately after that final result marker.

The marker order is therefore always:

```text
CUACOMPAT: BEGIN
CUACOMPAT: PASS|FAIL
CUACOMPAT: DONE
```

The reporter does not source or evaluate any user-owned file and does not run
`xprop`, `xrandr`, or Cua with root privileges.

## Ly and account invariants

The rest of Task 4 remains unchanged:

- Create locked UID 1000 `agent` only when UID 1000 is unused.
- Give it exactly the approved supplementary groups
  `audio,video,render,input,bluetooth,seat,hadron-agent-control`, never
  `admin`, `sudo`, or `docker`.
- Configure Ly for `agent`, `i3`, and `ly-autologin`.
- Clear inherited `ConditionPathExists` and `ConditionKernelCommandLine`
  conditions, then restore only
  `ConditionKernelCommandLine=!install-mode`.
- Preserve `ly@tty1.service` and Ly's existing `/usr/bin/X -keeptty` command.
- Do not add a hidden or nested display server.

## Failure behavior

Missing diagnostic binaries, invalid account/group state, a missing graphical
environment, diagnostic command failure, probe timeout, false probe result,
unsafe artifact input, missing artifact device, or tar-write failure is a test
failure. The reporter still makes a best-effort artifact write and emits
`DONE`, allowing the host harness to terminate the VM deterministically.

No failure falls back to Xvfb, a guessed display, a scraped process
environment, unsigned Flatpak content, or a reduced probe contract.

## Verification

Task 4 adds focused checks that prove:

- the original source/account contract is RED before implementation and GREEN
  afterward;
- source tarball digests are enforced;
- both diagnostic binaries exist only in the test image and have no unresolved
  links;
- shell syntax and the systemd unit shape are valid;
- the account is locked and has exactly the approved group set;
- Ly autologin values and the condition-reset drop-in are exact;
- the collector refuses a missing session environment;
- an injected `SECRET_TOKEN=sentinel` is represented only as
  `SECRET_TOKEN=<redacted>` and the sentinel never enters staged artifacts;
- the reporter's mocked success and timeout paths emit only the ordered marker
  protocol and attempt the artifact write at offset zero;
- production agent and ordinary desktop images remain free of the test
  account, fixtures, diagnostic clients, collector, reporter, and autologin
  override.

The real Ly/XLibre execution, graphical diagnostics, serial protocol, and raw
artifact disk are then exercised together by the visible QEMU gate in Phase 1
Tasks 6 and 7.

## Alternatives rejected

### Root consumes a session environment file

This uses fewer processes, but makes a privileged service parse user-owned
session values and then launch graphical clients on the user's behalf. Even
without `eval`, it creates an avoidable trust boundary and is harder to test.

### Override the session launcher and import a user-manager environment

This aligns with the eventual production appliance, but duplicates Phase 3's
broker lifecycle and user-manager design inside a compatibility-only task.
The split collector proves the same real-session invariant with less scope.
