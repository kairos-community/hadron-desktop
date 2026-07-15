#!/usr/bin/env bash
# Tests for hadron-agent-install + `hadron-agent provision inspect-seed`
# (Phase 3, Task 8): safe interactive AND explicit zero-touch installation.
#
# DISK-WIPE SAFETY is the whole point of this suite. The real install command
# is intercepted through the HADRON_INSTALL_CMD seam by a fake that records
# whether it ran and which device the written cloud-config told it to wipe. A
# test "installed" iff that fake ran; "did NOT install" iff it never ran.
#
# Cases:
#   1. sh -n syntax check.
#   2. no seed -> interactive prompt opens; a non-`yes` answer does NOT install.
#   3. rejection loops: typing anything but `yes` (incl. `y`) never installs;
#      a final abort exits WITHOUT writing a disk.
#   4. `yes` -> installs the SELECTED explicit disk; 99_hadron-agent-install.yaml
#      carries that device + enabled=true + the locked agent user; the fake
#      install cmd received the chosen device.
#   5. --noninteractive, seed with auto but NO device -> autoinstall FAILS.
#   6. --noninteractive, install.auto=false -> does NOT install.
#   7. --noninteractive, auto device + hadron_agent.enabled=true -> installs the
#      seed device.
#   8. --noninteractive, auto device but WITHOUT enabled -> fails (no install).
#   9. --noninteractive, an unrelated existing cloud-config (no install.auto) ->
#      does NOT authorize a wipe.
#  10. inspect-seed --quiet exit-code gate (ExecCondition contract) across the
#      absent / malformed / disabled / device-less / authorized seeds.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/rootfs-agent/usr/local/bin/hadron-agent-install"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ---------------------------------------------------------------------------
# 1. syntax
# ---------------------------------------------------------------------------
if sh -n "$script" 2>/dev/null; then
  ok "sh -n ${script#"$repo_root"/}"
else
  err "sh -n failed: ${script#"$repo_root"/}"
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# ---------------------------------------------------------------------------
# Build the real hadron-agent binary so --noninteractive exercises the real
# inspect-seed gate (not a stub).
# ---------------------------------------------------------------------------
agent_bin="$workdir/hadron-agent"
if ! ( cd "$repo_root/agent" && CGO_ENABLED=0 go build -o "$agent_bin" ./cmd/hadron-agent ) 2>"$workdir/build.log"; then
  echo "FATAL: could not build hadron-agent:" >&2
  cat "$workdir/build.log" >&2
  exit 1
fi
ok "built hadron-agent for inspect-seed"

# ---------------------------------------------------------------------------
# Fake install command (the HADRON_INSTALL_CMD seam). Records a call marker and
# the device the written cloud-config asks it to wipe. The real installer takes
# the device from install.device in /oem, so parsing it here proves the whole
# chain: script -> 99 yaml -> installer.
# ---------------------------------------------------------------------------
fake_install="$workdir/fake-install"
cat >"$fake_install" <<'EOF'
#!/bin/sh
: "${FAKE_CALL_LOG:?}" "${FAKE_DEVICE_FILE:?}" "${HADRON_OEM_DIR:?}"
echo "called $*" >>"$FAKE_CALL_LOG"
# Extract the device the installer would consume from the generated config.
dev=$(grep -E '^[[:space:]]*device:' "$HADRON_OEM_DIR/99_hadron-agent-install.yaml" 2>/dev/null \
        | head -n1 | sed -E 's/.*device:[[:space:]]*"?([^"[:space:]]+)"?.*/\1/')
printf '%s\n' "$dev" >"$FAKE_DEVICE_FILE"
exit 0
EOF
chmod +x "$fake_install"

# new_case NAME: create an isolated OEM dir + call/device markers for one case
# and export the seam env. Sets $CD to the case dir (must run in THIS shell, not
# a subshell, so the exports persist).
new_case() {
  CD="$workdir/$1"
  rm -rf "$CD"; mkdir -p "$CD/oem" "$CD/sysblock"
  export HADRON_OEM_DIR="$CD/oem"
  export FAKE_CALL_LOG="$CD/install.calls"
  export FAKE_DEVICE_FILE="$CD/install.device"
  export HADRON_SYS_BLOCK="$CD/sysblock"
  export HADRON_INSTALL_CMD="$fake_install"
  export HADRON_AGENT_BIN="$agent_bin"
  : >"$FAKE_CALL_LOG"
}

# fake_disk NAME SECTORS: create a fake whole-disk block device entry.
fake_disk() { mkdir -p "$HADRON_SYS_BLOCK/$1"; echo "$2" >"$HADRON_SYS_BLOCK/$1/size"; }

installed()     { [ -s "$FAKE_CALL_LOG" ]; }
device_wiped()  { [ -r "$FAKE_DEVICE_FILE" ] && cat "$FAKE_DEVICE_FILE"; }

run_interactive() { # feed stdin lines; returns script rc
  printf '%s' "$1" | HADRON_INSTALL_ALLOW_NOTTY=1 sh "$script" >"$2" 2>&1
}

# ---------------------------------------------------------------------------
# 2. no seed -> interactive prompt opens, and a non-yes answer does NOT install.
# ---------------------------------------------------------------------------
new_case case2; cd="$CD"; fake_disk sda 209715200
out="$cd/out"
# hostname=myhost, disk=1, confirm="no" -> loops, then EOF -> abort.
run_interactive "myhost
1
no
" "$out"; rc=$?
if grep -qi "Hostname" "$out"; then ok "no seed: interactive prompt opened"; else err "no seed: prompt did not open"; cat "$out"; fi
if installed; then err "no seed: MUST NOT install on a non-yes answer"; else ok "no seed: did not install"; fi
[ "$rc" -ne 0 ] && ok "no seed: aborted non-zero without installing" || err "no seed: expected non-zero abort"
[ -e "$HADRON_OEM_DIR/99_hadron-agent-install.yaml" ] && err "no seed: config written without confirmation" || ok "no seed: no config written"

# ---------------------------------------------------------------------------
# 3. rejection loops: `y` is not `yes`; final abort writes no disk.
# ---------------------------------------------------------------------------
new_case case3; cd="$CD"; fake_disk sda 209715200
out="$cd/out"
# Try "y" (must NOT count), then "nope", then EOF aborts.
run_interactive "h1
1
y
h2
1
nope
" "$out"; rc=$?
if installed; then err "rejection: 'y'/'nope' MUST NOT install"; else ok "rejection: 'y' and 'nope' did not install"; fi
if grep -qi "starting over" "$out"; then ok "rejection: re-prompted (looped) on non-yes"; else err "rejection: did not loop"; fi
[ -e "$HADRON_OEM_DIR/99_hadron-agent-install.yaml" ] && err "rejection: config written without a typed yes" || ok "rejection: no config written"

# ---------------------------------------------------------------------------
# 4. `yes` -> installs the SELECTED disk with the right config.
# ---------------------------------------------------------------------------
new_case case4; cd="$CD"; fake_disk sda 209715200; fake_disk sdb 419430400
out="$cd/out"
# pick disk #2 (=/dev/sdb), confirm exactly "yes".
run_interactive "workstation
2
yes
" "$out"; rc=$?
if installed; then ok "confirm: typed 'yes' installed"; else err "confirm: 'yes' did not install"; cat "$out"; fi
wiped="$(device_wiped)"
[ "$wiped" = "/dev/sdb" ] && ok "confirm: fake install received the SELECTED device (/dev/sdb)" \
  || err "confirm: install device = '$wiped', want /dev/sdb"
cfg="$HADRON_OEM_DIR/99_hadron-agent-install.yaml"
if [ -f "$cfg" ]; then
  grep -Eq '^[[:space:]]*device:[[:space:]]*"?/dev/sdb"?' "$cfg" && ok "config: explicit device /dev/sdb" || err "config: missing /dev/sdb device"
  grep -Eq '^[[:space:]]*auto:[[:space:]]*true'   "$cfg" && ok "config: install.auto true"   || err "config: missing auto:true"
  grep -Eq '^[[:space:]]*reboot:[[:space:]]*true' "$cfg" && ok "config: install.reboot true" || err "config: missing reboot:true"
  grep -Eq '^[[:space:]]*enabled:[[:space:]]*true' "$cfg" && ok "config: hadron_agent.enabled true" || err "config: missing hadron_agent enabled:true"
  grep -Eq 'name:[[:space:]]*"?agent"?' "$cfg" && ok "config: locked agent user present" || err "config: agent user missing"
  grep -Eq 'lock_passwd:[[:space:]]*true' "$cfg" && ok "config: agent password locked" || err "config: agent not locked"
  # Never grant the human agent a privileged group.
  if grep -Eq '(^|[^[:alnum:]_])(admin|sudo|docker)([^[:alnum:]_]|$)' "$cfg"; then
    err "config: privileged group leaked into agent user"
  else ok "config: no privileged groups for agent"; fi
else
  err "confirm: expected $cfg to be written"
fi
# The exact device shown in RED before confirmation.
esc=$(printf '\033')
if grep -q "${esc}\[1;31m" "$out" && grep -q "/dev/sdb" "$out"; then
  ok "confirm: selected device shown in RED"
else
  err "confirm: device not displayed in red"
fi

# ---------------------------------------------------------------------------
# Non-interactive (seed-gated) cases.
# ---------------------------------------------------------------------------
run_auto() { HADRON_HOSTNAME=ci sh "$script" --noninteractive >"$1" 2>&1; }

# 5. auto but NO device -> FAILS.
new_case case5; cd="$CD"; out="$cd/out"
cat >"$HADRON_OEM_DIR/50_seed.yaml" <<'EOF'
#cloud-config
install:
  auto: true
hadron_agent:
  enabled: true
EOF
run_auto "$out"; rc=$?
if installed; then err "auto-no-device: MUST NOT install"; else ok "auto-no-device: did not install"; fi
[ "$rc" -ne 0 ] && ok "auto-no-device: non-zero" || err "auto-no-device: expected non-zero"

# 6. install.auto=false -> does NOT install.
new_case case6; cd="$CD"; out="$cd/out"
cat >"$HADRON_OEM_DIR/50_seed.yaml" <<'EOF'
#cloud-config
install:
  auto: false
  device: /dev/sda
hadron_agent:
  enabled: true
EOF
run_auto "$out"; rc=$?
if installed; then err "auto-false: MUST NOT install"; else ok "auto-false: did not install"; fi

# 7. auto device + enabled -> installs the SEED device.
new_case case7; cd="$CD"; out="$cd/out"
cat >"$HADRON_OEM_DIR/50_seed.yaml" <<'EOF'
#cloud-config
install:
  auto: true
  device: /dev/vdb
hadron_agent:
  enabled: true
EOF
run_auto "$out"; rc=$?
if installed; then ok "authorized-auto: installed"; else err "authorized-auto: did not install"; cat "$out"; fi
wiped="$(device_wiped)"
[ "$wiped" = "/dev/vdb" ] && ok "authorized-auto: used the SEED device (/dev/vdb)" || err "authorized-auto: device=$wiped want /dev/vdb"
grep -Eq '^[[:space:]]*device:[[:space:]]*"?/dev/vdb"?' "$HADRON_OEM_DIR/99_hadron-agent-install.yaml" \
  && ok "authorized-auto: config carries the seed device" || err "authorized-auto: config missing seed device"

# 8. auto device WITHOUT enabled -> fails.
new_case case8; cd="$CD"; out="$cd/out"
cat >"$HADRON_OEM_DIR/50_seed.yaml" <<'EOF'
#cloud-config
install:
  auto: true
  device: /dev/sda
hadron_agent:
  enabled: false
EOF
run_auto "$out"; rc=$?
if installed; then err "auto-not-enabled: MUST NOT install"; else ok "auto-not-enabled: did not install"; fi

# 9. unrelated cloud-config (no install.auto) -> no authorization.
new_case case9; cd="$CD"; out="$cd/out"
cat >"$HADRON_OEM_DIR/50_seed.yaml" <<'EOF'
#cloud-config
hostname: someone-elses-box
users:
  - name: alice
    passwd: secret
stages:
  boot:
    - name: unrelated
EOF
run_auto "$out"; rc=$?
if installed; then err "unrelated: MUST NOT authorize a wipe"; else ok "unrelated: did not install"; fi

# ---------------------------------------------------------------------------
# 10. inspect-seed --quiet ExecCondition gate: only the fully-authorized seed
#     exits 0. This mirrors the autoinstall unit's ExecCondition exactly.
# ---------------------------------------------------------------------------
gate() { # SEED_CONTENT -> exit code of inspect-seed --require-auto-install --quiet
  _g="$workdir/gate"; rm -rf "$_g"; mkdir -p "$_g"
  printf '%s' "$1" >"$_g/50_seed.yaml"
  "$agent_bin" provision inspect-seed --require-auto-install --quiet --oem-dir "$_g" >"$_g/o" 2>&1
  _rc=$?
  [ -s "$_g/o" ] && err "gate: --quiet leaked output: $(cat "$_g/o")"
  return $_rc
}
gate "#cloud-config"$'\n' && err "gate absent: expected non-zero" || ok "gate: absent seed skipped (non-zero)"
gate "#cloud-config"$'\n'"install:"$'\n'"  auto: true"$'\n'"  device: [bad"$'\n' && err "gate malformed: expected non-zero" || ok "gate: malformed seed skipped"
gate "#cloud-config"$'\n'"install:"$'\n'"  auto: true"$'\n'"  device: /dev/sda"$'\n'"hadron_agent:"$'\n'"  enabled: false"$'\n' && err "gate disabled: expected non-zero" || ok "gate: disabled seed skipped"
gate "#cloud-config"$'\n'"install:"$'\n'"  auto: true"$'\n'"hadron_agent:"$'\n'"  enabled: true"$'\n' && err "gate device-less: expected non-zero" || ok "gate: device-less seed skipped"
if gate "#cloud-config"$'\n'"install:"$'\n'"  auto: true"$'\n'"  device: /dev/sda"$'\n'"hadron_agent:"$'\n'"  enabled: true"$'\n'; then
  ok "gate: fully-authorized seed passes (exit 0)"
else
  err "gate: fully-authorized seed should exit 0"
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: all install-script safety checks"
else
  echo "FAILED: one or more install-script safety checks failed" >&2
fi
exit "$fail"
