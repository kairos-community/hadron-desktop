#!/usr/bin/env bash
# Visible-desktop compatibility gate for the Hadron Cua agent appliance.
#
#   build i3 desktop -> agent overlay -> compat test image -> wrap with
#   AuroraBoot -> boot GRAPHICAL OVMF QEMU (virtio-vga + VNC) -> let the
#   in-guest probe drive the pinned cua-driver against the REAL XLibre/i3
#   session -> capture QEMU's actual scanout via QMP screendump -> extract the
#   guest artifact tar -> compare the driver's own screenshot against the VM
#   framebuffer -> pass/fail.
#
# This is the graphical sibling of test/run.sh (the M0 headless gate). The whole
# point of the phase is that the driver controls the DRM-backed XLibre desktop
# shown through QEMU VNC, so this harness NEVER uses -display none / -nographic.
#
# Usage:
#   test/agent/run.sh compatibility     # full build + boot + compare
#   SKIP_BUILD=1 ...                    # reuse the already-built images
#   SKIP_ISO=1   ...                    # reuse the already-built ISO
#   KEEP=1       ...                    # keep the per-run scratch (disk, sock, vars)
#
# Exit 0 = the driver controls the visible desktop and every capability passed.
set -uo pipefail

# --- locate repo root (two dirs up: test/agent -> test -> repo) --------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT" || exit 1

log() { echo -e "\n\033[1;34m[compat]\033[0m $*"; }
err() { echo -e "\033[1;31m[compat] $*\033[0m" >&2; }

# --- subcommand dispatch -----------------------------------------------------
SUBCOMMAND="${1:-}"
usage() {
  err "usage: $0 compatibility"
  err "  (compatibility is currently the only subcommand)"
}
case "$SUBCOMMAND" in
  compatibility) ;;
  "" ) usage; exit 64 ;;
  * ) err "unknown subcommand: $SUBCOMMAND"; usage; exit 64 ;;
esac

# --- image chain (confirmed against Makefile + the two Dockerfiles) ----------
# make DESKTOP=i3 image  => i3-desktop:dev  (IMAGE defaults to $(DESKTOP)-desktop:dev)
# Dockerfile.agent --target agent, BASE_IMAGE=i3-desktop:dev => hadron-agent:compat
# test/agent/Dockerfile.compat (final `test` stage), BASE_IMAGE=hadron-agent:compat
I3_IMAGE="i3-desktop:dev"
AGENT_IMAGE="hadron-agent:compat"
COMPAT_IMAGE="hadron-agent-compat:test"
AURORA_IMAGE="${AURORA_IMAGE:-quay.io/kairos/auroraboot:v0.21.0-alpha.4}"

# --- paths -------------------------------------------------------------------
WORK="$REPO_ROOT/build/agent-desktop"       # git-ignored scratch/work
ISO_DIR="$WORK/iso"
RUNTIME="$SCRIPT_DIR/runtime"               # git-ignored per-run scratch
ART="$SCRIPT_DIR/artifacts/compatibility"   # Task 7 reads result.json + frame-compare.json here
mkdir -p "$WORK" "$ISO_DIR" "$RUNTIME" "$ART"

CONSOLE="$ART/console.log"
QEMU_LOG="$ART/qemu.log"
PPM="$ART/qmp-desktop.ppm"
FRAME_JSON="$ART/frame-compare.json"
RESULT_JSON="$ART/result.json"
CUA_PNG="$ART/cua-desktop.png"

ARTDISK="$RUNTIME/artifacts.img"
QMP_SOCK="$RUNTIME/qmp.sock"
OVMF_VARS="$RUNTIME/OVMF_VARS.fd"

MEM="${MEM:-4096}"
CPUS="${CPUS:-4}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-600}"
VNC_DISPLAY="${VNC_DISPLAY:-19}"            # 127.0.0.1:19 -> TCP 5919

# ---------------------------------------------------------------------------
# 1. Build the image chain: i3 desktop -> agent overlay -> compat test image
# ---------------------------------------------------------------------------
if [ "${SKIP_BUILD:-0}" != "1" ]; then
  log "Building i3 desktop image ($I3_IMAGE)"
  make -C "$REPO_ROOT" DESKTOP=i3 image \
    || { err "i3 desktop image build failed"; exit 10; }

  log "Building agent overlay ($AGENT_IMAGE, Dockerfile.agent --target agent)"
  docker build -f "$REPO_ROOT/Dockerfile.agent" --target agent \
    --build-arg BASE_IMAGE="$I3_IMAGE" \
    -t "$AGENT_IMAGE" "$REPO_ROOT" \
    || { err "agent overlay build failed"; exit 11; }

  log "Building compat test image ($COMPAT_IMAGE, test/agent/Dockerfile.compat)"
  docker build -f "$REPO_ROOT/test/agent/Dockerfile.compat" \
    --build-arg BASE_IMAGE="$AGENT_IMAGE" \
    -t "$COMPAT_IMAGE" "$REPO_ROOT" \
    || { err "compat test image build failed"; exit 12; }
fi

# ---------------------------------------------------------------------------
# 2. Wrap the compat image in a bootable ISO with AuroraBoot
# ---------------------------------------------------------------------------
if [ "${SKIP_ISO:-0}" != "1" ]; then
  log "Building ISO with AuroraBoot"
  rm -f "$ISO_DIR"/*.iso
  docker run --rm --privileged \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$ISO_DIR":/output \
    "$AURORA_IMAGE" build-iso --output /output/ "docker:$COMPAT_IMAGE" \
    || { err "AuroraBoot ISO build failed"; exit 13; }
fi
ISO="$(ls -t "$ISO_DIR"/*.iso 2>/dev/null | head -1)"
[ -n "$ISO" ] || { err "no ISO produced"; exit 13; }
log "ISO: $ISO"

# ---------------------------------------------------------------------------
# 3. Create the 256 MiB raw artifact disk
# ---------------------------------------------------------------------------
# CROSS-TASK CONTRACT: the Task 4 reporter (cua-compat-report) writes its tar to
# /dev/disk/by-id/virtio-hadronagentartifacts. That by-id path only exists in
# the guest when this disk is attached as virtio-blk with serial EXACTLY
# "hadronagentartifacts". Do not change this serial.
ARTIFACTS_SERIAL="hadronagentartifacts"
log "Creating 256 MiB artifact disk ($ARTDISK, serial=$ARTIFACTS_SERIAL)"
qemu-img create -f raw "$ARTDISK" 256M >/dev/null \
  || { err "failed to create artifact disk"; exit 14; }

# ---------------------------------------------------------------------------
# 4. Locate OVMF (UEFI firmware) across distros
# ---------------------------------------------------------------------------
# The brief mandates OVMF: do NOT silently fall back to BIOS. Prefer a
# CODE+VARS split pair (persisted UEFI vars go in a writable per-run copy);
# fall back to a single combined image used as one writable pflash unit.
find_first() { for f in "$@"; do [ -f "$f" ] && { echo "$f"; return 0; }; done; return 1; }

OVMF_CODE="$(find_first \
  /usr/share/OVMF/OVMF_CODE_4M.fd \
  /usr/share/OVMF/OVMF_CODE.fd \
  /usr/share/edk2-ovmf/x64/OVMF_CODE.fd \
  /usr/share/edk2/x64/OVMF_CODE.fd \
  /usr/share/edk2/x64/OVMF_CODE.4m.fd \
  /usr/share/qemu/edk2-x86_64-code.fd || true)"
OVMF_VARS_TMPL="$(find_first \
  /usr/share/OVMF/OVMF_VARS_4M.fd \
  /usr/share/OVMF/OVMF_VARS.fd \
  /usr/share/edk2-ovmf/x64/OVMF_VARS.fd \
  /usr/share/edk2/x64/OVMF_VARS.fd \
  /usr/share/edk2/x64/OVMF_VARS.4m.fd \
  /usr/share/qemu/edk2-i386-vars.fd || true)"

OVMF_ARGS=()
if [ -n "$OVMF_CODE" ] && [ -n "$OVMF_VARS_TMPL" ]; then
  cp -f "$OVMF_VARS_TMPL" "$OVMF_VARS" \
    || { err "failed to copy OVMF vars template"; exit 14; }
  OVMF_ARGS=(
    -drive "if=pflash,format=raw,unit=0,readonly=on,file=$OVMF_CODE"
    -drive "if=pflash,format=raw,unit=1,file=$OVMF_VARS"
  )
  log "OVMF: split firmware CODE=$OVMF_CODE VARS=$OVMF_VARS_TMPL"
else
  OVMF_COMBINED="$(find_first \
    /usr/share/ovmf/OVMF.fd \
    /usr/share/OVMF/OVMF.fd \
    /usr/share/qemu/OVMF.fd \
    /usr/share/edk2/ovmf/OVMF.fd || true)"
  if [ -z "$OVMF_COMBINED" ]; then
    err "OVMF firmware not found. Install 'ovmf' (or 'edk2-ovmf'). This gate"
    err "requires UEFI/OVMF and will not fall back to BIOS."
    exit 14
  fi
  cp -f "$OVMF_COMBINED" "$OVMF_VARS" \
    || { err "failed to copy combined OVMF image"; exit 14; }
  OVMF_ARGS=(-drive "if=pflash,format=raw,unit=0,file=$OVMF_VARS")
  log "OVMF: combined firmware $OVMF_COMBINED"
fi

# ---------------------------------------------------------------------------
# 5. Boot the graphical OVMF VM
# ---------------------------------------------------------------------------
# tcg,thread=multi is a single QEMU argument; the comma is intentional.
# shellcheck disable=SC2054
ACCEL=(-accel tcg,thread=multi)
if [ -e /dev/kvm ]; then
  ACCEL=(-enable-kvm -cpu host)
  log "KVM available: using -enable-kvm -cpu host"
else
  log "No /dev/kvm: using -accel tcg,thread=multi"
fi

log "Booting graphical QEMU (VNC 127.0.0.1:$VNC_DISPLAY, timeout ${BOOT_TIMEOUT}s)"
rm -f "$CONSOLE" "$QMP_SOCK"
touch "$CONSOLE"

# virtio-vga + -vnc gives the real DRM-backed scanout we screendump later.
# NEVER add -display none / -nographic here: that would defeat the phase.
qemu-system-x86_64 \
  "${ACCEL[@]}" \
  -m "$MEM" -smp "$CPUS" \
  "${OVMF_ARGS[@]}" \
  -device virtio-vga \
  -vnc "127.0.0.1:$VNC_DISPLAY" \
  -serial "file:$CONSOLE" \
  -qmp "unix:$QMP_SOCK,server,nowait" \
  -rtc base=utc,clock=rt \
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0 \
  -drive "if=none,id=artifacts,format=raw,file=$ARTDISK" \
  -device "virtio-blk-pci,drive=artifacts,serial=$ARTIFACTS_SERIAL" \
  -cdrom "$ISO" \
  -boot d \
  >"$QEMU_LOG" 2>&1 &
QEMU_PID=$!

# Make sure the VM never outlives the harness, even on Ctrl-C.
# shellcheck disable=SC2329  # invoked indirectly via the trap below.
cleanup() {
  [ -n "${QEMU_PID:-}" ] && kill "$QEMU_PID" 2>/dev/null
  wait "$QEMU_PID" 2>/dev/null
  if [ "${KEEP:-0}" != "1" ]; then
    rm -f "$ARTDISK" "$QMP_SOCK" "$OVMF_VARS"
  fi
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 6. Wait for the guest to finish (CUACOMPAT: DONE, QEMU exit, or timeout)
# ---------------------------------------------------------------------------
deadline=$((SECONDS + BOOT_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  if grep -q "CUACOMPAT: DONE" "$CONSOLE" 2>/dev/null; then
    log "Guest reported CUACOMPAT: DONE"
    break
  fi
  if ! kill -0 "$QEMU_PID" 2>/dev/null; then
    log "QEMU exited before DONE"
    break
  fi
  sleep 3
done

# ---------------------------------------------------------------------------
# 7. Capture QEMU's real scanout via QMP screendump (before stopping the VM)
# ---------------------------------------------------------------------------
# QMP screendump with a .ppm filename writes a P6 PPM of the current framebuffer.
# Best-effort: if it fails we still extract guest artifacts so a FAIL stays
# diagnosable (the missing PPM later fails frame_compare, which is the point).
rm -f "$PPM"
if kill -0 "$QEMU_PID" 2>/dev/null; then
  log "Requesting QMP screendump -> $PPM"
  if python3 - "$QMP_SOCK" "$PPM" <<'PY'
import json
import socket
import sys
import time

sock_path, out_path = sys.argv[1], sys.argv[2]

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(15)
for _ in range(20):
    try:
        s.connect(sock_path)
        break
    except OSError:
        time.sleep(0.5)
else:
    print("qmp: cannot connect to socket", file=sys.stderr)
    sys.exit(1)

buf = b""


def recv_json():
    global buf
    while b"\n" not in buf:
        chunk = s.recv(4096)
        if not chunk:
            raise EOFError("qmp connection closed")
        buf += chunk
    line, buf = buf.split(b"\n", 1)
    return json.loads(line.decode())


def send(obj):
    s.sendall((json.dumps(obj) + "\r\n").encode())


def wait_reply():
    # QMP interleaves asynchronous events; skip them until a command reply.
    while True:
        msg = recv_json()
        if "return" in msg or "error" in msg:
            return msg


recv_json()  # server greeting: {"QMP": {...}}
send({"execute": "qmp_capabilities"})
reply = wait_reply()
if "error" in reply:
    print("qmp_capabilities error:", reply, file=sys.stderr)
    sys.exit(1)

send({"execute": "screendump", "arguments": {"filename": out_path}})
reply = wait_reply()
if "error" in reply:
    print("screendump error:", reply, file=sys.stderr)
    sys.exit(1)

s.close()
PY
  then
    log "QMP screendump written"
  else
    err "QMP screendump failed (continuing so the FAIL is diagnosable)"
  fi
else
  err "QEMU not running; cannot screendump"
fi

# ---------------------------------------------------------------------------
# 8. Stop the VM, then extract the guest artifact tar from the raw disk
# ---------------------------------------------------------------------------
kill "$QEMU_PID" 2>/dev/null
wait "$QEMU_PID" 2>/dev/null

# cua-compat-report builds the tar with `tar -C <stage> -cf <tar> .` and dd's it
# to offset 0 of this disk, so entries are top-level (./result.json,
# ./cua-desktop.png, ...). Extracting into $ART lands result.json and
# cua-desktop.png directly at $ART/result.json and $ART/cua-desktop.png.
if tar -tf "$ARTDISK" >/dev/null 2>&1; then
  if tar -xf "$ARTDISK" -C "$ART" 2>/dev/null; then
    log "Extracted guest artifacts to $ART"
  else
    err "guest artifact tar present but extraction reported errors"
  fi
else
  err "no readable guest artifact tar on $ARTDISK"
fi

# ---------------------------------------------------------------------------
# 9. Compare the driver's own screenshot against QEMU's real scanout
# ---------------------------------------------------------------------------
# Produced here (Task 7 only READS frame-compare.json). Run it before the marker
# gates below so the artifact exists even when we then exit on a FAIL marker.
FRAME_OK=0
if [ -s "$CUA_PNG" ] && [ -s "$PPM" ]; then
  log "Comparing $CUA_PNG against $PPM"
  if python3 "$SCRIPT_DIR/frame_compare.py" "$CUA_PNG" "$PPM" "$FRAME_JSON"; then
    FRAME_OK=1
  else
    err "framebuffer comparison did not pass"
  fi
else
  err "cannot compare frames: missing $CUA_PNG and/or $PPM"
fi

# ---------------------------------------------------------------------------
# 10. Evaluate serial markers and the probe result matrix
# ---------------------------------------------------------------------------
log "Markers from $CONSOLE:"
grep "CUACOMPAT:" "$CONSOLE" 2>/dev/null | sed 's/^/    /' || true

if ! grep -q "CUACOMPAT: BEGIN" "$CONSOLE" 2>/dev/null; then
  err "RESULT: FAIL - guest never reached the reporter (no BEGIN). See $CONSOLE / $QEMU_LOG"
  exit 2
fi
if grep -q "CUACOMPAT: FAIL" "$CONSOLE" 2>/dev/null; then
  err "RESULT: FAIL - reporter emitted CUACOMPAT: FAIL."
  exit 1
fi
if ! grep -q "CUACOMPAT: DONE" "$CONSOLE" 2>/dev/null; then
  err "RESULT: FAIL - guest did not finish (no DONE; likely hung/timeout)."
  exit 3
fi

# result.json is a flat object of capability booleans: fail on ANY false field.
if [ ! -s "$RESULT_JSON" ]; then
  err "RESULT: FAIL - no result.json extracted from the guest."
  exit 4
fi
if command -v jq >/dev/null 2>&1; then
  if ! jq -e 'to_entries | all(.value == true)' "$RESULT_JSON" >/dev/null 2>&1; then
    err "RESULT: FAIL - one or more capability fields are false:"
    jq -r 'to_entries[] | select(.value != true) | "    \(.key) = \(.value)"' \
      "$RESULT_JSON" >&2 2>/dev/null || true
    exit 4
  fi
else
  # Fallback: any `: false` in the flat boolean object means a failed capability.
  if grep -Eq ':[[:space:]]*false' "$RESULT_JSON"; then
    err "RESULT: FAIL - one or more capability fields are false (grep fallback):"
    grep -E ':[[:space:]]*false' "$RESULT_JSON" | sed 's/^/    /' >&2 || true
    exit 4
  fi
fi

if [ "$FRAME_OK" != "1" ]; then
  err "RESULT: FAIL - the driver's screenshot does not match the visible framebuffer."
  exit 5
fi

log "RESULT: PASS - the pinned cua-driver controls the visible XLibre/i3 desktop."
exit 0
