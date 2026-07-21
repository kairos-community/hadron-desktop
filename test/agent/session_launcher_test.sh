#!/usr/bin/env bash
# Tests for the agent session launcher (Phase 3, Task 6).
#
#   1. `sh -n` syntax-checks start-desktop and hadron-agent-session-ready.
#   2. Drives hadron-agent-session-ready with a FAKE `systemctl` recorder and a
#      controlled environment, asserting that on a correct session it imports
#      EXACTLY `DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS` and restarts EXACTLY
#      `hadron-agent-session.service`, and that it REFUSES (no systemctl calls)
#      when a session var is missing or when DBUS points at a foreign bus.
#   3. Structural assertions on start-desktop: it must NOT call dbus-run-session
#      and MUST call hadron-agent-session-ready before exec-ing i3.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
bin="$repo_root/rootfs-agent/usr/bin"
launcher="$bin/start-desktop"
ready="$bin/hadron-agent-session-ready"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ---------------------------------------------------------------------------
# 1. syntax
# ---------------------------------------------------------------------------
for f in "$launcher" "$ready"; do
  if sh -n "$f" 2>/dev/null; then
    ok "sh -n ${f#$repo_root/}"
  else
    err "sh -n failed: ${f#$repo_root/}"
  fi
done

# ---------------------------------------------------------------------------
# 2. hadron-agent-session-ready behavior with a fake systemctl
# ---------------------------------------------------------------------------
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# Fake systemctl records each invocation (one line of args per call).
fakebin="$workdir/bin"
mkdir -p "$fakebin"
record="$workdir/systemctl.calls"
cat >"$fakebin/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$record"
exit 0
EOF
chmod +x "$fakebin/systemctl"

uid="$(id -u)"
good_bus="unix:path=/run/user/${uid}/bus"

# run_ready ENVSPEC...: runs the script with PATH shadowed by the fake systemctl
# and only the given env vars set. Resets the recorder first. Echoes exit code.
run_ready() {
  : >"$record"
  env -i PATH="$fakebin:/usr/bin:/bin" "$@" sh "$ready" >/dev/null 2>&1
  echo $?
}

# 2a. Correct session: imports exactly the three vars, restarts exactly the unit.
code="$(run_ready DISPLAY=:0 XAUTHORITY=/home/agent/.Xauthority DBUS_SESSION_BUS_ADDRESS="$good_bus")"
if [ "$code" -ne 0 ]; then
  err "session-ready exited $code on a valid session"
else
  ok "session-ready succeeds on a valid session"
fi

# The recorder captures the full arg vector, including the `--user` scope.
if grep -qx -- '--user import-environment DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS' "$record"; then
  ok "imports EXACTLY DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS"
else
  err "import line was: '$(grep import-environment "$record" || true)'"
fi

if grep -qx -- '--user restart hadron-agent-session.service' "$record"; then
  ok "restarts EXACTLY hadron-agent-session.service"
else
  err "restart line was: '$(grep restart "$record" || true)'"
fi

# Exactly two systemctl calls, nothing else.
calls="$(wc -l <"$record" | tr -d ' ')"
if [ "$calls" = "2" ]; then
  ok "issued exactly 2 systemctl calls"
else
  err "expected 2 systemctl calls, got $calls: $(cat "$record")"
fi

# It must never scrape another process: it must not read any /proc entry.
if grep -q '/proc' "$ready"; then
  err "session-ready reads /proc (must never scrape another process's environ)"
else
  ok "session-ready never reads /proc (no environment scraping)"
fi

# 2b. Wrong bus (not the user-manager bus): must refuse, no systemctl calls.
code="$(run_ready DISPLAY=:0 XAUTHORITY=/home/agent/.Xauthority DBUS_SESSION_BUS_ADDRESS='unix:path=/tmp/other/bus')"
if [ "$code" -eq 0 ]; then
  err "session-ready accepted a foreign DBUS bus"
else
  ok "session-ready refuses a foreign DBUS bus (exit $code)"
fi
if [ -s "$record" ]; then
  err "session-ready called systemctl despite a foreign bus: $(cat "$record")"
else
  ok "no systemctl calls on a foreign bus"
fi

# 2c. Missing each required var in turn: must refuse.
code="$(run_ready XAUTHORITY=/x DBUS_SESSION_BUS_ADDRESS="$good_bus")"        # no DISPLAY
[ "$code" -ne 0 ] && ok "refuses when DISPLAY missing" || err "accepted missing DISPLAY"
code="$(run_ready DISPLAY=:0 DBUS_SESSION_BUS_ADDRESS="$good_bus")"           # no XAUTHORITY
[ "$code" -ne 0 ] && ok "refuses when XAUTHORITY missing" || err "accepted missing XAUTHORITY"
code="$(run_ready DISPLAY=:0 XAUTHORITY=/x)"                                  # no DBUS
[ "$code" -ne 0 ] && ok "refuses when DBUS_SESSION_BUS_ADDRESS missing" || err "accepted missing DBUS"

# ---------------------------------------------------------------------------
# 3. start-desktop structure: shared bus (NO dbus-run-session), ready before i3
#    Structural greps run over the CODE only (comments stripped) so a mention of
#    dbus-run-session or i3 in a comment cannot fool the check.
# ---------------------------------------------------------------------------
code_only="$workdir/start-desktop.code"
grep -vE '^[[:space:]]*#' "$launcher" >"$code_only"

if grep -q 'dbus-run-session' "$code_only"; then
  err "start-desktop calls dbus-run-session (would isolate Cua from the shared user bus)"
else
  ok "start-desktop does NOT call dbus-run-session (shares the user-manager bus)"
fi

if grep -q 'hadron-agent-session-ready' "$code_only"; then
  ok "start-desktop calls hadron-agent-session-ready"
else
  err "start-desktop never calls hadron-agent-session-ready"
fi

# session-ready must be invoked BEFORE `exec i3`.
ready_ln="$(grep -n 'hadron-agent-session-ready' "$code_only" | head -1 | cut -d: -f1)"
exec_ln="$(grep -n 'exec i3' "$code_only" | head -1 | cut -d: -f1)"
if [ -n "$ready_ln" ] && [ -n "$exec_ln" ] && [ "$ready_ln" -lt "$exec_ln" ]; then
  ok "session-ready runs before exec i3 (code line $ready_ln < $exec_ln)"
else
  err "session-ready must precede exec i3 (ready=$ready_ln exec=$exec_ln)"
fi

# It must point DBUS at the user-manager bus and wait for that bus socket.
if grep -q 'DBUS_SESSION_BUS_ADDRESS="unix:path=' "$code_only" && grep -q '/run/user/' "$code_only"; then
  ok "start-desktop targets the user-manager bus (/run/user/<uid>/bus)"
else
  err "start-desktop does not target /run/user/.../bus"
fi
if grep -Eq '\-S ' "$code_only"; then
  ok "start-desktop waits for the user-manager bus socket"
else
  err "start-desktop does not wait for the bus socket"
fi

# ---------------------------------------------------------------------------
# 4. Unit / drop-in shapes: watchdog install-mode exclusion + Ly agent profile.
# ---------------------------------------------------------------------------
wd="$repo_root/rootfs-agent/etc/systemd/system/hadron-agent-display-watchdog.service"
lydrop="$repo_root/rootfs-agent/etc/systemd/system/ly@.service.d/20-agent-profile.conf"

if [ -f "$wd" ]; then
  ok "display-watchdog unit exists"
  grep -q 'ConditionKernelCommandLine=!install-mode' "$wd" \
    && ok "watchdog: ConditionKernelCommandLine=!install-mode" \
    || err "watchdog: missing ConditionKernelCommandLine=!install-mode"
  grep -Eq 'After=.*hadron-agent-provision\.service.*ly@tty1\.service' "$wd" \
    && ok "watchdog: After=hadron-agent-provision.service ly@tty1.service" \
    || err "watchdog: missing After ordering"
  grep -q 'ExecStart=/usr/bin/hadron-agent display-watchdog --user agent --grace 30s' "$wd" \
    && ok "watchdog: ExecStart display-watchdog --user agent --grace 30s" \
    || err "watchdog: wrong ExecStart"
  # It must never launch a display itself.
  if grep -Eq 'ExecStart=.*(Xvfb|/usr/bin/X |startx|xinit| i3 )' "$wd"; then
    err "watchdog unit launches a display directly"
  else
    ok "watchdog unit launches no display (only restarts ly@tty1 at runtime)"
  fi
else
  err "missing display-watchdog unit: $wd"
fi

if [ -f "$lydrop" ]; then
  ok "Ly agent drop-in exists"
  # Clears both inherited conditions, then sets ONLY !install-mode.
  grep -qx 'ConditionPathExists=' "$lydrop" \
    && ok "ly drop-in clears inherited ConditionPathExists" \
    || err "ly drop-in does not clear ConditionPathExists"
  grep -qx 'ConditionKernelCommandLine=' "$lydrop" \
    && ok "ly drop-in clears inherited ConditionKernelCommandLine" \
    || err "ly drop-in does not clear ConditionKernelCommandLine"
  grep -qx 'ConditionKernelCommandLine=!install-mode' "$lydrop" \
    && ok "ly drop-in sets ConditionKernelCommandLine=!install-mode" \
    || err "ly drop-in does not set !install-mode"
  # It must NOT re-add the live_mode guard as a DIRECTIVE (agent logs in on a
  # live boot too); a mention in a comment is fine.
  if grep -vE '^[[:space:]]*#' "$lydrop" | grep -q 'live_mode'; then
    err "ly drop-in re-adds a live_mode guard (would block the live agent login)"
  else
    ok "ly drop-in does not block the live agent login"
  fi
else
  err "missing Ly agent drop-in: $lydrop"
fi

# ---------------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
  echo "PASS: session launcher tests"
else
  echo "FAILED: session launcher tests" >&2
fi
exit "$fail"
