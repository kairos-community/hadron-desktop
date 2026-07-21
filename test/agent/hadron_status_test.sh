#!/usr/bin/env bash
# Tests for the agent-aware hadron-status override and its i3 wiring
# (Phase 3, Task 7).
#
#   1. `sh -n` syntax-checks the script.
#   2. All five "Agent: <State>" mappings, fed via a fake status.json:
#      Starting, Ready, Active, Paused, Degraded.
#   3. A missing status.json degrades gracefully to "Agent: Starting".
#   4. Poll cadence: sleep 1 while Active/Paused, sleep 5 otherwise (verified
#      with a recording fake `sleep` on PATH).
#   5. The base net/vol/mem/date fields are still present (the override only
#      prepends, it does not drop the inherited output).
#   6. The exact i3 binding line for the emergency chord is present in
#      90-hadron-agent.conf, and the first-run exec is present exactly once
#      (not duplicated from Task 6).
#
# HADRON_STATUS_MAX_ITER=1 (or 2, for the cadence check) is a test-only seam
# in the script itself so a single iteration of the otherwise-infinite loop
# can be captured deterministically, with no backgrounding/killing needed.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
bin="$repo_root/rootfs-agent/usr/bin"
script="$bin/hadron-status"
i3conf="$repo_root/rootfs-agent/etc/i3/config.d/90-hadron-agent.conf"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ---------------------------------------------------------------------------
# 1. syntax
# ---------------------------------------------------------------------------
if sh -n "$script" 2>/dev/null; then
  ok "sh -n ${script#$repo_root/}"
else
  err "sh -n failed: ${script#$repo_root/}"
fi

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

fakebin="$workdir/bin"
mkdir -p "$fakebin"
sleep_log="$workdir/sleep.log"

# Fake sleep: records the requested duration and returns immediately, so the
# tests never actually wait out a real 1s/5s interval.
cat >"$fakebin/sleep" <<EOF
#!/bin/sh
echo "\$1" >>"$sleep_log"
exit 0
EOF
chmod +x "$fakebin/sleep"

# run_status STATUS_JSON_CONTENT_OR_EMPTY MAX_ITER: writes (or removes) a
# status.json fixture, runs hadron-status for MAX_ITER iterations, and
# echoes the LAST printed line. sleep.log is reset first.
status_json="$workdir/status.json"
run_status() {
  local content="$1" max_iter="$2"
  if [ -n "$content" ]; then
    printf '%s' "$content" >"$status_json"
  else
    rm -f "$status_json"
  fi
  : >"$sleep_log"
  env -i PATH="$fakebin:/usr/bin:/bin" \
    HADRON_AGENT_STATUS_JSON="$status_json" \
    HADRON_STATUS_MAX_ITER="$max_iter" \
    sh "$script" 2>&1 | tail -n1
}

json_ready='{"gateway":true,"session":true,"cua":true,"paused":false,"degraded":false,"computer_use_active":false,"ready":true}'
json_active='{"gateway":true,"session":true,"cua":true,"paused":false,"degraded":false,"computer_use_active":true,"ready":true}'
json_paused='{"gateway":true,"session":true,"cua":true,"paused":true,"degraded":false,"computer_use_active":false,"ready":true}'
json_degraded='{"gateway":true,"session":false,"cua":true,"paused":false,"degraded":true,"computer_use_active":false,"ready":false}'
json_starting='{"gateway":false,"session":false,"cua":false,"paused":false,"degraded":false,"computer_use_active":false,"ready":false}'

# ---------------------------------------------------------------------------
# 2 + 3. exact state mappings, including missing status.json
# ---------------------------------------------------------------------------
assert_state() {
  local desc="$1" content="$2" want="$3"
  local line
  line="$(run_status "$content" 1)"
  case "$line" in
    *"Agent: $want"*)
      # Must be an exact token: "Agent: Ready" must not also match when the
      # state is "Ready2" or similar -- assert the char right after the state
      # word is a space or pipe (end of token), never alnum.
      if printf '%s' "$line" | grep -Eq "Agent: ${want}([^A-Za-z]|\$)"; then
        ok "$desc -> exact 'Agent: $want' prefix"
      else
        err "$desc -> 'Agent: $want' matched but not as an exact token: $line"
      fi
      ;;
    *)
      err "$desc -> expected 'Agent: $want', got: $line"
      ;;
  esac
}

assert_state "ready status.json"    "$json_ready"    "Ready"
assert_state "active status.json"   "$json_active"   "Active"
assert_state "paused status.json"   "$json_paused"   "Paused"
assert_state "degraded status.json" "$json_degraded" "Degraded"
assert_state "starting status.json" "$json_starting" "Starting"
assert_state "missing status.json"  ""               "Starting"

# ---------------------------------------------------------------------------
# 4. poll cadence
# ---------------------------------------------------------------------------
assert_cadence() {
  local desc="$1" content="$2" want="$3"
  run_status "$content" 2 >/dev/null
  if grep -qx "$want" "$sleep_log"; then
    ok "$desc -> polls every ${want}s"
  else
    err "$desc -> expected a sleep($want) call, sleep.log was: $(cat "$sleep_log")"
  fi
}

assert_cadence "ready"    "$json_ready"    5
assert_cadence "active"   "$json_active"   1
assert_cadence "paused"   "$json_paused"   1
assert_cadence "degraded" "$json_degraded" 5
assert_cadence "starting" ""               5

# ---------------------------------------------------------------------------
# 5. base fields survive the override
# ---------------------------------------------------------------------------
line="$(run_status "$json_ready" 1)"
case "$line" in
  *"Vol "*"Mem "*)
    ok "base Vol/Mem fields still present in the overridden output"
    ;;
  *)
    err "base Vol/Mem fields missing from overridden output: $line"
    ;;
esac

# ---------------------------------------------------------------------------
# 6. i3 wiring: emergency chord binding + first-run exec, no duplication
# ---------------------------------------------------------------------------
if [ -f "$i3conf" ]; then
  ok "90-hadron-agent.conf exists"

  bindsym_count=$(grep -cFx 'bindsym $mod+Shift+Escape exec --no-startup-id /usr/bin/hadron-agent control toggle' "$i3conf")
  if [ "$bindsym_count" -eq 1 ]; then
    ok "emergency chord binding present exactly once"
  else
    err "emergency chord binding present $bindsym_count times (want exactly 1): $i3conf"
  fi

  exec_count=$(grep -cFx 'exec --no-startup-id /usr/bin/hadron-agent-first-run' "$i3conf")
  if [ "$exec_count" -eq 1 ]; then
    ok "first-run panel exec present exactly once"
  else
    err "first-run panel exec present $exec_count times (want exactly 1): $i3conf"
  fi
else
  err "missing i3 drop-in: $i3conf"
fi

# ---------------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
  echo "PASS: hadron-status tests"
else
  echo "FAILED: hadron-status tests" >&2
fi
exit "$fail"
