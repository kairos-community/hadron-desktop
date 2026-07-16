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
  err "usage: $0 <compatibility|fixture [ISO]|stop>"
  err "  compatibility  build + boot the graphical QEMU compatibility gate"
  err "  fixture [ISO]  boot a detachable, VNC-visible appliance VM and emit"
  err "                 a descriptor pointing at its MCP/VNC/QMP endpoints"
  err "  stop           gracefully power down the running fixture, keep artifacts"
}

# Progress for the fixture/stop subcommands goes to STDERR only: the `fixture`
# subcommand's STDOUT must carry NOTHING but the descriptor path.
finfo() { echo -e "\033[1;34m[fixture]\033[0m $*" >&2; }

# _fixture_paths -- deterministic runtime/artifact locations shared by both
# `fixture` (writer) and `stop` (reader). Overridable via FIXTURE_RUNTIME /
# FIXTURE_ART so a caller can run several fixtures side by side.
_fixture_paths() {
  FIXTURE_RUNTIME="${FIXTURE_RUNTIME:-$SCRIPT_DIR/runtime/fixture}"
  FIXTURE_ART="${FIXTURE_ART:-$SCRIPT_DIR/artifacts/fixture}"
  FIXTURE_QEMU_PID="$FIXTURE_RUNTIME/qemu.pid"     # qemu's own -pidfile
  FIXTURE_LAUNCH_PID="$FIXTURE_RUNTIME/launch.pid" # backup pid captured at spawn
  FIXTURE_NOVNC_PID="$FIXTURE_RUNTIME/novnc.pid"
  FIXTURE_QMP_SOCK="$FIXTURE_RUNTIME/qmp.sock"
}

# cmd_fixture [ISO] -- boot a long-lived, VNC-visible appliance VM whose MCP
# gateway is forwarded to a loopback host port, TOFU-capture its self-signed
# leaf, verify /readyz through the pinned CA, emit the descriptor, print ONLY
# the descriptor path, and stay running until `stop` powers the guest down.
cmd_fixture() {
  shift  # drop "fixture"
  local iso="${1:-${FIXTURE_ISO:-}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"
  _fixture_paths

  for bin in qemu-system-x86_64 openssl curl python3; do
    command -v "$bin" >/dev/null 2>&1 || { err "required command not found: $bin"; return 1; }
  done

  [ -n "$iso" ] || iso="$(ls -t "$REPO_ROOT/build/agent-desktop/iso"/*.iso 2>/dev/null | head -1 || true)"
  [ -n "$iso" ] && [ -f "$iso" ] || {
    err "no appliance ISO found. Pass one: $0 fixture path/to.iso  (or set FIXTURE_ISO)"
    return 1
  }

  mkdir -p "$FIXTURE_RUNTIME" "$FIXTURE_ART"
  chmod 700 "$FIXTURE_RUNTIME" "$FIXTURE_ART" 2>/dev/null || true

  # --- ports / display (loopback only) --------------------------------------
  local mcp_port vnc_display vnc_port novnc_port
  mcp_port="$(hdn_agent_alloc_port)"
  vnc_display="${FIXTURE_VNC:-25}"
  vnc_port=$((5900 + vnc_display))
  novnc_port="${FIXTURE_NOVNC_PORT:-6090}"

  # --- per-run files --------------------------------------------------------
  local disk="$FIXTURE_RUNTIME/disk.qcow2"
  local serial_log="$FIXTURE_ART/serial.log"
  local boot_log="$FIXTURE_ART/qemu-launch.log"
  local descriptor="$FIXTURE_ART/fixture.json"
  local CA_CERT="$FIXTURE_ART/ca.pem"
  local FINGERPRINT_FILE="$FIXTURE_ART/tls-fingerprint.txt"
  local MCP_PORT="$mcp_port"

  # --- mint per-VM credentials + cidata seed ISO (Task 2) -------------------
  # make-seed.sh stdout is KEY=VALUE and carries NO bearer plaintext; still
  # route it through redaction as defense in depth. We consume USER_TOKEN_FILE
  # and SEED_ISO.
  local seed_dir="$FIXTURE_RUNTIME/seed" seed_out="$FIXTURE_RUNTIME/make-seed.env"
  finfo "Minting fixture credentials + cidata seed ISO"
  if ! "$SCRIPT_DIR/make-seed.sh" "$seed_dir" \
        --mode install --device /dev/vda --listen 0.0.0.0:7443 \
        >"$seed_out" 2>"$FIXTURE_ART/make-seed.log"; then
    err "make-seed.sh failed (see $FIXTURE_ART/make-seed.log)"
    return 1
  fi
  local user_token_file seed_iso
  user_token_file="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  seed_iso="$(sed -n 's/^SEED_ISO=//p' "$seed_out")"
  [ -n "$user_token_file" ] && [ -f "$user_token_file" ] || { err "make-seed produced no USER_TOKEN_FILE"; return 1; }
  [ -n "$seed_iso" ] && [ -f "$seed_iso" ] || { err "make-seed produced no SEED_ISO"; return 1; }

  # --- launch QEMU via tools/vm.sh with the detachable-fixture knobs --------
  # tools/vm.sh execs qemu, so the backgrounded shell's PID becomes qemu's PID.
  rm -f "$FIXTURE_QEMU_PID" "$FIXTURE_QMP_SOCK"
  finfo "Booting fixture VM (MCP 127.0.0.1:$mcp_port -> guest :7443, VNC 127.0.0.1:$vnc_port)"
  HOST_MCP_PORT="$mcp_port" \
  QMP="$FIXTURE_QMP_SOCK" \
  SERIAL_LOG="$serial_log" \
  PID_FILE="$FIXTURE_QEMU_PID" \
  SEED_ISO="$seed_iso" \
  DISK="$disk" DISK_SIZE="${FIXTURE_DISK_SIZE:-24G}" FRESH=1 \
  DESKTOP=i3 \
  VNC="$vnc_display" BIND=127.0.0.1 \
  MEM="${MEM:-4096}" CPUS="${CPUS:-4}" \
    "$REPO_ROOT/tools/vm.sh" install "$iso" >"$boot_log" 2>&1 &
  local qemu_pid=$!
  hdn_agent_track_pid "$qemu_pid"
  printf '%s' "$qemu_pid" > "$FIXTURE_LAUNCH_PID"

  # --- optional noVNC web console (managed here so `stop` can reap it) -------
  local novnc_url=""
  if [ "${NOVNC:-0}" = "1" ]; then
    local ws novnc_root
    ws="$(command -v websockify || echo "$HOME/.local/bin/websockify")"
    novnc_root="$(hdn_fixture_novnc_root || true)"
    if [ -x "$ws" ] && [ -n "$novnc_root" ]; then
      "$ws" --web "$novnc_root" "127.0.0.1:$novnc_port" "127.0.0.1:$vnc_port" \
        >"$FIXTURE_ART/novnc.log" 2>&1 &
      local ws_pid=$!
      hdn_agent_track_pid "$ws_pid"
      printf '%s' "$ws_pid" > "$FIXTURE_NOVNC_PID"
      novnc_url="http://127.0.0.1:$novnc_port/vnc.html?autoconnect=1"
      finfo "noVNC: $novnc_url"
    else
      err "NOVNC=1 but websockify or a noVNC checkout was not found; serving plain VNC only"
    fi
  fi

  # --- wait for the forwarded gateway port to come up -----------------------
  local timeout="${FIXTURE_BOOT_TIMEOUT:-900}"
  local deadline=$((SECONDS + timeout))
  finfo "Waiting up to ${timeout}s for the guest gateway on 127.0.0.1:$mcp_port"
  local up=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! kill -0 "$qemu_pid" 2>/dev/null; then
      err "QEMU exited before the gateway came up (see $boot_log / $serial_log)"
      return 1
    fi
    if hdn_fixture_tcp_open 127.0.0.1 "$mcp_port"; then up=1; break; fi
    sleep 3
  done
  [ "$up" = "1" ] || { err "timed out waiting for 127.0.0.1:$mcp_port"; return 1; }

  # --- TOFU: capture the self-signed leaf, pin it, verify /readyz -----------
  # The appliance leaf has SAN 127.0.0.1 (Phase 3), so --cacert verifies
  # against 127.0.0.1 with NO -k / insecure skip.
  finfo "Capturing the self-signed leaf (TOFU) and pinning its fingerprint"
  local cap=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    openssl s_client -connect "127.0.0.1:$MCP_PORT" -showcerts </dev/null \
      2>/dev/null | openssl x509 -out "$CA_CERT" 2>/dev/null || true
    if [ -s "$CA_CERT" ]; then cap=1; break; fi
    sleep 2
  done
  [ "$cap" = "1" ] || { err "could not capture a leaf certificate from 127.0.0.1:$MCP_PORT"; return 1; }
  chmod 600 "$CA_CERT" 2>/dev/null || true
  openssl x509 -in "$CA_CERT" -outform DER |
    sha256sum | awk '{print $1}' > "$FINGERPRINT_FILE"

  finfo "Waiting for /readyz to return 200 through the pinned CA"
  local ready=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl --fail --silent --show-error --cacert "$CA_CERT" \
         "https://127.0.0.1:$MCP_PORT/readyz" >/dev/null 2>&1; then
      ready=1; break
    fi
    sleep 3
  done
  [ "$ready" = "1" ] || { err "gateway /readyz never returned 200 (see $serial_log)"; return 1; }

  local fingerprint
  fingerprint="$(tr -d '[:space:]' < "$FINGERPRINT_FILE")"

  # --- emit the descriptor atomically (0600, absolute paths, no token) ------
  hdn_fixture_emit_descriptor "$descriptor" \
    "https://127.0.0.1:$MCP_PORT/mcp" \
    "$user_token_file" \
    "$CA_CERT" \
    "$fingerprint" \
    "127.0.0.1:$vnc_port" \
    "$novnc_url" \
    "$FIXTURE_QMP_SOCK" \
    "$FIXTURE_ART" \
    || { err "descriptor emission failed"; return 1; }

  finfo "Fixture ready. Descriptor: $descriptor"
  finfo "  MCP:  https://127.0.0.1:$MCP_PORT/mcp"
  finfo "  VNC:  127.0.0.1:$vnc_port   QMP: $FIXTURE_QMP_SOCK"
  finfo "  stop with: $0 stop"

  # STDOUT carries ONLY the descriptor path.
  printf '%s\n' "$descriptor"

  # Stay running (the VM is long-lived); `stop` powers it down, which makes
  # this wait return. The EXIT trap from common.sh reaps any tracked PID that
  # is somehow still alive.
  wait "$qemu_pid" 2>/dev/null || true
  return 0
}

# cmd_stop -- read the runtime PID, request a graceful QMP powerdown, escalate
# to a kill after 20s, stop noVNC, and KEEP all artifacts.
cmd_stop() {
  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"
  _fixture_paths

  local qpid=""
  if [ -f "$FIXTURE_QEMU_PID" ]; then
    qpid="$(tr -dc '0-9' < "$FIXTURE_QEMU_PID")"
  fi
  if [ -z "$qpid" ] && [ -f "$FIXTURE_LAUNCH_PID" ]; then
    qpid="$(tr -dc '0-9' < "$FIXTURE_LAUNCH_PID")"
  fi

  if [ -z "$qpid" ]; then
    err "no fixture PID file under $FIXTURE_RUNTIME; nothing to stop"
  elif ! kill -0 "$qpid" 2>/dev/null; then
    finfo "fixture PID $qpid is not running; cleaning up"
    qpid=""
  fi

  # 1. graceful ACPI powerdown via QMP.
  if [ -S "$FIXTURE_QMP_SOCK" ]; then
    finfo "Requesting graceful QMP powerdown"
    qmp_system_powerdown "$FIXTURE_QMP_SOCK" || err "QMP powerdown request failed; will escalate"
  else
    err "QMP socket $FIXTURE_QMP_SOCK missing; cannot request graceful powerdown"
  fi

  # 2. wait up to 20s, then escalate.
  if [ -n "$qpid" ]; then
    local waited=0
    while [ "$waited" -lt 20 ] && kill -0 "$qpid" 2>/dev/null; do
      sleep 1
      waited=$((waited + 1))
    done
    if kill -0 "$qpid" 2>/dev/null; then
      err "guest still up after 20s; escalating to SIGTERM/SIGKILL on PID $qpid"
      kill -TERM "$qpid" 2>/dev/null || true
      sleep 2
      kill -KILL "$qpid" 2>/dev/null || true
    else
      finfo "guest powered down gracefully"
    fi
  fi

  # 3. stop noVNC.
  if [ -f "$FIXTURE_NOVNC_PID" ]; then
    local wpid
    wpid="$(tr -dc '0-9' < "$FIXTURE_NOVNC_PID")"
    [ -n "$wpid" ] && kill "$wpid" 2>/dev/null || true
    rm -f "$FIXTURE_NOVNC_PID"
  fi

  # 4. drop runtime control files; KEEP the artifact directory.
  rm -f "$FIXTURE_QEMU_PID" "$FIXTURE_LAUNCH_PID" "$FIXTURE_QMP_SOCK"
  finfo "Fixture stopped; artifacts kept in $FIXTURE_ART"
  return 0
}

case "$SUBCOMMAND" in
  compatibility) ;;  # falls through to the compatibility gate body below
  fixture) cmd_fixture "$@"; exit $? ;;
  stop)    cmd_stop    "$@"; exit $? ;;
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
# 2b. Extract the live kernel + initrd from the ISO for a direct-kernel boot
# ---------------------------------------------------------------------------
# We boot QEMU with -kernel/-initrd so the HARNESS controls the kernel cmdline
# (live mode, no install-mode, no nomodeset, no vga=795) instead of deferring to
# the ISO GRUB default entry, whose cmdline breaks the phase (install-mode stops
# Ly; nomodeset kills virtio-gpu KMS). The host has no xorriso/bsdtar/7z and no
# passwordless sudo, so extraction runs inside the AuroraBoot image, which is
# already present locally (it built the ISO) and ships xorriso/osirrox. The
# image's entrypoint is the auroraboot binary, so we override it with xorriso.
KDIR="$RUNTIME/kboot"
KERNEL="$KDIR/kernel"
INITRD="$KDIR/initrd"
rm -rf "$KDIR"
mkdir -p "$KDIR"
log "Extracting /boot/kernel + /boot/initrd from ISO via xorriso ($AURORA_IMAGE)"
docker run --rm \
  --entrypoint xorriso \
  -v "$ISO":/iso.iso:ro \
  -v "$KDIR":/out \
  "$AURORA_IMAGE" \
  -osirrox on -indev /iso.iso \
  -extract /boot/kernel /out/kernel \
  -extract /boot/initrd /out/initrd \
  || { err "kernel/initrd extraction (xorriso) failed"; exit 15; }
[ -s "$KERNEL" ] && [ -s "$INITRD" ] \
  || { err "extraction produced no kernel/initrd (kernel=$KERNEL initrd=$INITRD)"; exit 15; }
log "Extracted kernel ($(stat -c%s "$KERNEL") B) + initrd ($(stat -c%s "$INITRD") B)"

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

log "Booting graphical QEMU via direct kernel (VNC 127.0.0.1:$VNC_DISPLAY, timeout ${BOOT_TIMEOUT}s)"
rm -f "$CONSOLE" "$QMP_SOCK"
touch "$CONSOLE"

# Direct-kernel live boot. The ISO is attached as a READ-ONLY virtio-blk disk
# (NOT `-cdrom`): under direct-kernel boot the emulated IDE/ATAPI CD-ROM is
# never presented to Linux (no sr0), so the initrd's `root=live:CDLABEL=COS_LIVE`
# label scan finds nothing and drops to dracut emergency. virtio-blk is the one
# storage driver this initrd is proven to bring up (it detects the artifact
# disk), and `CDLABEL=` resolves by filesystem label via udev, so it works on
# any block device carrying the ISO9660 label. We drop `-boot d` and supply our
# own kernel/initrd/cmdline.
#
# We use the ISO "Kairos" default entry's `rd.cos.disable` path (plain
# dmsquash-live: the initrd just scans block devices for CDLABEL=COS_LIVE), which
# is the boot path we verified reaches userspace/multi-user under QEMU. We do NOT
# use the immucore `kairos.boot_live_mode` path: immucore looks for the boot
# medium it was launched from, which a direct `-kernel` boot never provides, so
# it drops to dracut emergency (initqueue timeout on COS_LIVE).
#
# From that proven cmdline we drop only the three flags that break this phase:
#   - install-mode  (stops Ly autologin via the !install-mode condition)
#   - nomodeset     (blocks virtio-gpu KMS -> no DRM scanout, virtio_gpu -EINVAL)
#   - vga=795       (legacy vesafb hint that can fight KMS)
# Do NOT re-add nomodeset / install-mode / vga=795.
KERNEL_CMDLINE="cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs rd.live.overlay.overlayfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable selinux=0"

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
  -drive "if=none,id=livecd,format=raw,readonly=on,file=$ISO" \
  -device "virtio-blk-pci,drive=livecd,serial=coslive" \
  -kernel "$KERNEL" \
  -initrd "$INITRD" \
  -append "$KERNEL_CMDLINE" \
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
#
# Guard against stale-artifact spurious PASS: result.json / cua-desktop.png /
# frame-compare.json (and every other file a prior run's guest tar dropped
# into $ART, e.g. environment.txt, cua-driver-doctor.json, xprop-root.txt,
# xrandr.txt, collection.status, report.log, gtk-state.json, chromium.commit,
# user-journal.log) are ONLY ever produced by this extraction step or by the
# frame-compare step right after it. SKIP_BUILD=1/SKIP_ISO=1 reuse the same
# $ART across runs, so if extraction silently fails below but the serial still
# shows PASS/DONE, a leftover result.json/cua-desktop.png from a previous run
# would make the checks further down pass on stale data. Clear every
# guest/derived artifact now, before extraction, so this run starts clean.
# Host-side captures that were freshly (re)written earlier in THIS run
# (console log, qemu log, the QMP PPM) are preserved.
log "Clearing stale guest artifacts from $ART before extraction"
find "$ART" -mindepth 1 -maxdepth 1 \
  ! -name "$(basename "$CONSOLE")" \
  ! -name "$(basename "$QEMU_LOG")" \
  ! -name "$(basename "$PPM")" \
  -exec rm -rf {} +

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

# ---------------------------------------------------------------------------
# 11. Record the compatibility descriptor (Task 7). This is a generated artifact
#     under the git-ignored artifacts dir; it is never committed. Best-effort:
#     a descriptor hiccup must not fail an otherwise-green gate.
# ---------------------------------------------------------------------------
if command -v jq >/dev/null 2>&1; then
  COMPAT_JSON="$ART/compatibility.json"
  # Pins mirror Dockerfile.agent / test/agent/Dockerfile.compat build args.
  cua_revision="3cadb5f82e7d2ed071a2082764276ec872a52135"
  atspi_version="2.54.0"
  cua_version="$(jq -r '(.probes[]?|select(.label=="binary")|.message)//""' "$ART/cua-driver-doctor.json" 2>/dev/null)"
  xlibre_version="$(grep -aoE 'XLibre X Server [0-9][0-9.]*' "$ART/user-journal.log" 2>/dev/null | head -1 | awk '{print $NF}')"
  chromium_commit="$(cat "$ART/chromium.commit" 2>/dev/null)"
  image_id="$(docker image inspect --format '{{.Id}}' "$COMPAT_IMAGE" 2>/dev/null)"
  if jq -n \
      --arg cua_revision "$cua_revision" \
      --arg cua_version "$cua_version" \
      --arg xlibre_version "$xlibre_version" \
      --arg atspi_version "$atspi_version" \
      --arg chromium_commit "$chromium_commit" \
      --arg image_id "$image_id" \
      --slurpfile frame "$FRAME_JSON" \
      '{cua_revision:$cua_revision, cua_version:$cua_version, xlibre_version:$xlibre_version,
        atspi_version:$atspi_version, chromium_flatpak_commit:$chromium_commit, image_id:$image_id,
        cua_width:($frame[0].cua_width), cua_height:($frame[0].cua_height),
        qmp_width:($frame[0].qmp_width), qmp_height:($frame[0].qmp_height),
        frame_abs_error:($frame[0].error)}' \
      > "$COMPAT_JSON" 2>/dev/null; then
    log "Wrote compatibility descriptor: $COMPAT_JSON"
  else
    err "warning: could not write compatibility.json (gate still PASS)"
  fi
fi

log "RESULT: PASS - the pinned cua-driver controls the visible XLibre/i3 desktop."
exit 0
