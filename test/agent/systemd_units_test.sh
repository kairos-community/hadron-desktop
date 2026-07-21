#!/usr/bin/env bash
# Unit-shape tests for the Hadron agent service lifecycle (Phase 3, Task 5).
#
# Two layers:
#   1. Structural grep assertions: every unit file exists, has valid INI section
#      structure, and carries the exact directives the brief mandates (isolation
#      for the gateway, condition guards for provision/root-helper/session, the
#      restart backoff, the SIGHUP reloads, Delegate, etc.). Plus the tmpfiles
#      socket-dir owners/modes and the OEM account/linger stage.
#   2. If `systemd-analyze` is available, run `systemd-analyze verify` on each
#      system unit in an isolated unit dir. verify emits warnings for the missing
#      ExecStart binary and for unknown ordering deps (hadron-agent-autoinstall,
#      ly@tty1) -- those are TOLERATED. A directive-syntax error is FATAL.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
agent_root="$repo_root/rootfs-agent"
sys="$agent_root/etc/systemd/system"
usr="$agent_root/usr/lib/systemd/user"
tmpfiles="$agent_root/etc/tmpfiles.d/hadron-agent.conf"
oem="$agent_root/system/oem/90_agent_profile.yaml"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

have() { command -v "$1" >/dev/null 2>&1; }

assert_file() {
  [ -f "$1" ] || { err "missing file: $1"; return 1; }
  ok "exists: ${1#$repo_root/}"
}

# grep -F (fixed string) must be present.
need() {
  local file="$1" needle="$2"
  if grep -Fq -- "$needle" "$file"; then
    ok "${file##*/}: has '$needle'"
  else
    err "${file##*/}: missing '$needle'"
  fi
}

# grep -E (regex) must be present.
need_re() {
  local file="$1" re="$2"
  if grep -Eq -- "$re" "$file"; then
    ok "${file##*/}: matches /$re/"
  else
    err "${file##*/}: missing /$re/"
  fi
}

# A directive (key at line start, ignoring comments) must NOT be present.
# Comments/prose that merely mention the key are ignored.
deny_directive() {
  local file="$1" key="$2"
  if grep -vE '^[[:space:]]*#' "$file" | grep -Eq "^[[:space:]]*${key}"; then
    err "${file##*/}: must NOT set directive '${key}'"
  else
    ok "${file##*/}: correctly lacks directive '${key}'"
  fi
}

# A word must NOT appear on any non-comment line (used for forbidden group names).
deny_word() {
  local file="$1" word="$2"
  if grep -vE '^[[:space:]]*#' "$file" | grep -Ewq -- "$word"; then
    err "${file##*/}: must NOT reference '${word}' in an executable line"
  else
    ok "${file##*/}: correctly lacks '${word}' outside comments"
  fi
}

# Minimal INI validation: every non-comment, non-blank line before the first
# section header is illegal; every key=value line must sit under a [Section];
# every [Section] header must be well-formed.
validate_ini() {
  local file="$1"
  awk -v F="$file" '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    /^\[.*\]$/       { sec=1; next }
    /^\[/ && !/\]$/  { print "FAIL: " F ": malformed section header: " $0 > "/dev/stderr"; bad=1; next }
    {
      if (!sec) { print "FAIL: " F ": key outside any section: " $0 > "/dev/stderr"; bad=1; next }
      if ($0 !~ /=/) { print "FAIL: " F ": non key=value line: " $0 > "/dev/stderr"; bad=1 }
    }
    END { exit bad ? 1 : 0 }
  ' "$file" || return 1
  ok "${file##*/}: INI structure valid"
}

# --- 1. files exist -------------------------------------------------------
provision="$sys/hadron-agent-provision.service"
gateway="$sys/hadron-agent-gateway.service"
roothelper="$sys/hadron-agent-root-helper.service"
session="$usr/hadron-agent-session.service"

for f in "$provision" "$gateway" "$roothelper" "$session" "$tmpfiles" "$oem"; do
  assert_file "$f" || true
done

for u in "$provision" "$gateway" "$roothelper" "$session"; do
  validate_ini "$u" || true
done

# --- 2. provision --------------------------------------------------------
need    "$provision" "Type=oneshot"
need    "$provision" "RemainAfterExit=yes"
need    "$provision" "ConditionPathExists=/etc/hadron-agent/profile"
need_re "$provision" '^Before=.*hadron-agent-gateway\.service'
need_re "$provision" '^Before=.*ly@tty1\.service'
need_re "$provision" '^After=.*network-online\.target'
need_re "$provision" '^After=.*hadron-agent-autoinstall\.service'
need_re "$provision" '^Wants=.*network-online\.target'
need    "$provision" "hadron-agent provision --oem-dir /oem --state-dir /var/lib/hadron-agent --runtime-dir /run/hadron-agent"
deny_directive "$provision" "User="   # runs as root

# --- 3. gateway (the hardened, internet-facing unit) ---------------------
need    "$gateway" "User=hadron-agent-gateway"
need    "$gateway" "Group=hadron-agent-gateway"
need    "$gateway" "NoNewPrivileges=yes"
need    "$gateway" "ProtectSystem=strict"
need    "$gateway" "ProtectHome=yes"
need    "$gateway" "PrivateTmp=yes"
need    "$gateway" "PrivateDevices=yes"
need_re "$gateway" '^CapabilityBoundingSet=[[:space:]]*$'   # empty = drop all
need_re "$gateway" '^AmbientCapabilities=[[:space:]]*$'
need_re "$gateway" '^SupplementaryGroups=[[:space:]]*$'     # no extra groups
need    "$gateway" "ReadWritePaths=/run/hadron-agent/status /run/hadron-agent/control"
need    "$gateway" "Restart=on-failure"
need    "$gateway" "RestartSec=2s"
need    "$gateway" "RestartSteps=5"
need    "$gateway" "RestartMaxDelaySec=30s"
need    "$gateway" "ExecReload=/bin/kill -HUP \$MAINPID"
need_re "$gateway" '^ExecStart=/usr/bin/hadron-agent gateway'
need_re "$gateway" '^After=.*hadron-agent-provision\.service'
# Never grant the gateway a privileged group.
deny_word "$gateway" "admin"
deny_word "$gateway" "docker"

# --- 4. root helper (root-equivalent; deliberately NOT sandboxed) --------
need    "$roothelper" "ConditionPathExists=/var/lib/hadron-agent/root/enabled"
need    "$roothelper" "Delegate=yes"
need_re "$roothelper" '^ExecStart=/usr/bin/hadron-agent root-helper'
need    "$roothelper" "ExecReload=/bin/kill -HUP \$MAINPID"
need    "$roothelper" "Restart=on-failure"
# It must NOT be sandboxed like the gateway.
deny_directive "$roothelper" "RestrictAddressFamilies"
deny_directive "$roothelper" "ProtectSystem="
deny_directive "$roothelper" "NoNewPrivileges="
deny_directive "$roothelper" "PrivateDevices="
deny_directive "$roothelper" "User="   # runs as root

# --- 5. session user unit ------------------------------------------------
need    "$session" "ConditionUser=agent"
need    "$session" "Delegate=yes"
need_re "$session" '^ExecStart=/usr/bin/hadron-agent session'
need    "$session" "Restart=on-failure"
need    "$session" "RestartSec=2s"
need    "$session" "RestartMaxDelaySec=30s"
# Display tolerance: no DISPLAY-dependent ExecStartPre gating startup.
deny_directive "$session" "ExecStartPre"

# --- 6. tmpfiles socket dir owners/modes ---------------------------------
need_re "$tmpfiles" '^d[[:space:]]+/run/hadron-agent/session[[:space:]]+0750[[:space:]]+agent[[:space:]]+hadron-agent-gateway'
need_re "$tmpfiles" '^d[[:space:]]+/run/hadron-agent/root[[:space:]]+0750[[:space:]]+root[[:space:]]+hadron-agent-gateway'
need_re "$tmpfiles" '^d[[:space:]]+/run/hadron-agent/control[[:space:]]+0750[[:space:]]+hadron-agent-gateway[[:space:]]+hadron-agent-control'
need_re "$tmpfiles" '^d[[:space:]]+/run/hadron-agent/status[[:space:]]+0750[[:space:]]+hadron-agent-gateway[[:space:]]+agent'
# Crucial: the root socket dir is root-owned, so `agent` cannot unlink/replace
# the root socket. Assert agent is neither owner nor owning-group of /root.
if grep -E '^d[[:space:]]+/run/hadron-agent/root' "$tmpfiles" | grep -Eq '(^|[[:space:]])agent([[:space:]]|$)'; then
  err "tmpfiles: agent must NOT own or group the root socket dir"
else
  ok "tmpfiles: agent is excluded from the root socket dir"
fi

# --- 7. OEM account/linger stage -----------------------------------------
need    "$oem" "loginctl enable-linger agent"
need    "$oem" "useradd"
need    "$oem" "--uid 1000"
need    "$oem" "UID 1000 already used"          # abort-on-conflict guard
need    "$oem" "install-mode"                   # skip during installer
need    "$oem" "/etc/hadron-agent/profile"      # profile-marker guard
need    "$oem" "audio video render input bluetooth seat hadron-agent-control"
need    "$oem" "systemd-tmpfiles --create /etc/tmpfiles.d/hadron-agent.conf"
# Never grant the human agent a privileged group (on any executable line).
deny_word "$oem" "admin"
deny_word "$oem" "sudo"
deny_word "$oem" "docker"

# --- 8. optional: systemd-analyze verify ---------------------------------
if have systemd-analyze; then
  echo "--- systemd-analyze verify ---"
  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT
  cp "$provision" "$gateway" "$roothelper" "$session" "$workdir/"
  verify_out="$workdir/verify.log"
  # verify each unit; capture stderr. Missing binaries / unknown ordering units
  # are expected warnings, not failures.
  for u in hadron-agent-provision.service hadron-agent-gateway.service \
           hadron-agent-root-helper.service hadron-agent-session.service; do
    systemd-analyze verify "$workdir/$u" >"$verify_out" 2>&1 || true
    # Keep only findings about THIS unit (drop host-unit scan noise like
    # mnt-*.mount permission errors), then drop the tolerated classes: the
    # ExecStart binary is absent in this test env, and ordering deps
    # (hadron-agent-autoinstall, ly@tty1) are not present either.
    residual="$(grep -E "^${u}:" "$verify_out" \
      | grep -Ev 'is not executable|No such file|not found|does not exist|Unknown ordering|Cannot add dependency' || true)"
    # A real directive/syntax error looks like these:
    if printf '%s\n' "$residual" | grep -Eq 'Unknown lvalue|Unknown key|Unknown section|Invalid|Failed to parse|[Ss]yntax|Assignment outside'; then
      echo "--- verify output for $u ---" >&2
      cat "$verify_out" >&2
      err "verify reported a directive/syntax error in $u"
    else
      ok "verify: no directive/syntax errors in $u (tolerated warnings only)"
    fi
  done
else
  echo "note: systemd-analyze not available; ran grep/INI checks only"
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: all systemd unit-shape checks"
else
  echo "FAILED: one or more unit-shape checks failed" >&2
fi
exit "$fail"
