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
  err "usage: $0 <compatibility|contract [IMAGE]|install [IMAGE]|generic [ISO]|fixture [IMAGE]|stop>"
  err "  compatibility    build + boot the graphical QEMU compatibility gate"
  err "  contract [IMAGE] NON-DESTRUCTIVE live gate: boot the agent live against a"
  err "                   blank disk (proven unwritten) and run the mcp-smoke contract"
  err "  install [IMAGE]  zero-touch install gate: build a provisioning ISO, auto-install,"
  err "                   run the contract suite, and prove a reboot-persistent marker"
  err "  generic [ISO]    boot a plain desktop ISO live and prove it is non-destructive"
  err "  fixture [IMAGE]  boot a detachable, VNC-visible appliance VM and emit"
  err "                   a descriptor pointing at its MCP/VNC/QMP endpoints"
  err "  stop             gracefully power down the running fixture, keep artifacts"
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

# ===========================================================================
# Shared helpers for the gate subcommands (contract / install / generic /
# fixture). They assume lib/common.sh + lib/qmp.sh + lib/fixture.sh are already
# sourced by the caller. Arrays cannot be returned from a function, so the
# helpers that build QEMU argument arrays set shell globals (AGENT_ACCEL,
# AGENT_OVMF_ARGS) the caller then splices into its qemu command line.
# ===========================================================================

# The AuroraBoot image is the single tool that builds the provisioning ISO and
# extracts the live kernel/initrd. Overridable via AURORA_IMAGE.
AGENT_AURORA_IMAGE="${AURORA_IMAGE:-quay.io/kairos/auroraboot:v0.21.0-alpha.4}"

# _agent_require_bins BIN... -- fail if any listed command is missing.
_agent_require_bins() {
  local bin ok=0
  for bin in "$@"; do
    command -v "$bin" >/dev/null 2>&1 || { err "required command not found: $bin"; ok=1; }
  done
  return "$ok"
}

# _agent_find_first PATH... -- print the first path that exists, or fail.
_agent_find_first() {
  local f
  for f in "$@"; do [ -f "$f" ] && { printf '%s' "$f"; return 0; }; done
  return 1
}

# _agent_sha256 <file> -- print the file's lowercase-hex SHA-256.
_agent_sha256() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

# _agent_set_accel -- KVM when available, else multi-threaded TCG.
_agent_set_accel() {
  if [ -e /dev/kvm ]; then
    AGENT_ACCEL=(-enable-kvm -cpu host)
  else
    # tcg,thread=multi is one argument; the comma is QEMU syntax.
    # shellcheck disable=SC2054
    AGENT_ACCEL=(-accel tcg,thread=multi)
  fi
}

# _agent_find_ovmf <vars_out> -- locate OVMF firmware, copy a fresh writable
# VARS image to <vars_out>, and populate AGENT_OVMF_ARGS. UEFI/OVMF is
# mandatory: this never falls back to BIOS.
_agent_find_ovmf() {
  local vars_out="$1" code vars combined
  code="$(_agent_find_first \
    /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd \
    /usr/share/edk2-ovmf/x64/OVMF_CODE.fd /usr/share/edk2/x64/OVMF_CODE.fd \
    /usr/share/edk2/x64/OVMF_CODE.4m.fd /usr/share/qemu/edk2-x86_64-code.fd || true)"
  vars="$(_agent_find_first \
    /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/OVMF/OVMF_VARS.fd \
    /usr/share/edk2-ovmf/x64/OVMF_VARS.fd /usr/share/edk2/x64/OVMF_VARS.fd \
    /usr/share/edk2/x64/OVMF_VARS.4m.fd /usr/share/qemu/edk2-i386-vars.fd || true)"
  if [ -n "$code" ] && [ -n "$vars" ]; then
    cp -f "$vars" "$vars_out" || { err "failed to copy OVMF vars template"; return 1; }
    AGENT_OVMF_ARGS=(
      -drive "if=pflash,format=raw,unit=0,readonly=on,file=$code"
      -drive "if=pflash,format=raw,unit=1,file=$vars_out"
    )
    return 0
  fi
  combined="$(_agent_find_first /usr/share/ovmf/OVMF.fd /usr/share/OVMF/OVMF.fd \
    /usr/share/qemu/OVMF.fd /usr/share/edk2/ovmf/OVMF.fd || true)"
  [ -n "$combined" ] || { err "OVMF firmware not found; install 'ovmf'/'edk2-ovmf' (this gate needs UEFI, no BIOS fallback)"; return 1; }
  cp -f "$combined" "$vars_out" || { err "failed to copy combined OVMF image"; return 1; }
  AGENT_OVMF_ARGS=(-drive "if=pflash,format=raw,unit=0,file=$vars_out")
}

# _agent_build_prov_iso <cloud_config> <iso_out_dir> <image> -- build a
# provisioning ISO from the local container <image> with <cloud_config> embedded
# via AuroraBoot's --cloud-config. AuroraBoot writes the config to the ISO root
# as /config.yaml, mounted at /run/initramfs/live/config.yaml at runtime -- the
# delivery the agent config loader + inspect-seed actually read (a cidata seed,
# by contrast, lands at /oem/95_userdata with no .yaml suffix and is NOT read).
# Prints ONLY the built ISO path on stdout.
_agent_build_prov_iso() {
  local cfg="$1" out_dir="$2" image="$3" iso
  mkdir -p "$out_dir"
  rm -f "$out_dir"/*.iso
  docker run --rm --privileged \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$out_dir":/output \
    -v "$cfg":/config.yaml:ro \
    "$AGENT_AURORA_IMAGE" build-iso --output /output/ \
    --cloud-config /config.yaml "docker:$image" >&2 \
    || { err "auroraboot ISO build failed"; return 1; }
  iso="$(ls -t "$out_dir"/*.iso 2>/dev/null | head -1 || true)"
  [ -n "$iso" ] || { err "auroraboot produced no ISO"; return 1; }
  printf '%s' "$iso"
}

# _agent_extract_kernel_initrd <iso> <kdir> -- extract /boot/kernel +
# /boot/initrd from <iso> into <kdir> via xorriso inside the AuroraBoot image
# (the host has no xorriso). Needed for the direct-kernel live boot, whose
# harness-controlled cmdline selects kairos.boot_live_mode.
_agent_extract_kernel_initrd() {
  local iso="$1" kdir="$2"
  rm -rf "$kdir"; mkdir -p "$kdir"
  docker run --rm --entrypoint xorriso \
    -v "$iso":/iso.iso:ro -v "$kdir":/out "$AGENT_AURORA_IMAGE" \
    -osirrox on -indev /iso.iso \
    -extract /boot/kernel /out/kernel -extract /boot/initrd /out/initrd >/dev/null 2>&1 \
    || { err "kernel/initrd extraction failed"; return 1; }
  [ -s "$kdir/kernel" ] && [ -s "$kdir/initrd" ] \
    || { err "extraction produced no kernel/initrd"; return 1; }
}

# _agent_inject_cloud_config <iso> <config> <out_iso> -- remaster <iso> so that
# <config> lands at the ISO root as /config.yaml, which is where a live/install
# boot mounts it (/run/initramfs/live/config.yaml) and where kairos-agent looks.
#
# This exists because a RELEASED ISO carries no cloud-config: `make agent-iso`
# builds the appliance without one, so a guest booted from it finds no
# provisioned bearer digests and mints a throwaway first-run token instead. The
# symptom is nasty precisely because it looks fine -- /readyz goes green while
# the caller's own bearer gets 401. Downstream consumers are handed an ISO URL
# and their credentials separately, so the two have to be married here.
#
# `-boot_image any replay` is not optional: it replays the El Torito and EFI
# boot records from the input image. Without it the remastered ISO is a valid
# filesystem that no firmware will boot.
_agent_inject_cloud_config() {
  local iso="$1" cfg="$2" out="$3"
  rm -f "$out"
  mkdir -p "$(dirname "$out")"
  docker run --rm --entrypoint xorriso \
    -v "$iso":/in.iso:ro -v "$cfg":/config.yaml:ro \
    -v "$(dirname "$out")":/out "$AGENT_AURORA_IMAGE" \
    -indev /in.iso -outdev "/out/$(basename "$out")" \
    -boot_image any replay \
    -map /config.yaml /config.yaml >/dev/null 2>&1 \
    || { err "cloud-config injection failed"; return 1; }
  [ -s "$out" ] || { err "cloud-config injection produced no ISO"; return 1; }
}

# _agent_build_mcp_smoke <out_bin> -- build the mcp-smoke client. Its main
# package lives under test/agent/cmd/mcp-smoke but imports the agent module's
# internal/smoke package, so it must be COPIED into the module tree
# (agent/cmd/mcp-smoke) before `go build`. The copy is removed on process exit.
# Honors a prebuilt MCP_SMOKE_BIN. Prints ONLY the binary path.
_agent_build_mcp_smoke() {
  local out="$1" moddir
  if [ -n "${MCP_SMOKE_BIN:-}" ] && [ -x "$MCP_SMOKE_BIN" ]; then
    printf '%s' "$MCP_SMOKE_BIN"; return 0
  fi
  command -v go >/dev/null 2>&1 || { err "go toolchain required to build mcp-smoke (or set MCP_SMOKE_BIN)"; return 1; }
  moddir="$REPO_ROOT/agent/cmd/mcp-smoke"
  mkdir -p "$moddir"
  cp -f "$SCRIPT_DIR/cmd/mcp-smoke/main.go" "$moddir/main.go" \
    || { err "could not stage mcp-smoke into the module"; return 1; }
  hdn_agent_on_exit "rm -rf '$moddir'"
  ( cd "$REPO_ROOT/agent" && CGO_ENABLED=0 go build -trimpath -o "$out" ./cmd/mcp-smoke ) >&2 \
    || { err "mcp-smoke build failed"; return 1; }
  printf '%s' "$out"
}

# _agent_screenshot <sock> <ppm_out> -- best-effort QMP screendump to <ppm_out>
# plus a sibling .png if PIL is present. Never fails the caller.
_agent_screenshot() {
  local sock="$1" ppm="$2"
  [ -S "$sock" ] || return 0
  qmp_screendump "$sock" "$ppm" 2>/dev/null || { err "screendump failed (continuing)"; return 0; }
  python3 -c "from PIL import Image; Image.open('$ppm').save('${ppm%.ppm}.png')" 2>/dev/null || true
}

# _agent_tofu <host> <port> <ca_out> <fp_out> <deadline_epoch> -- capture the
# self-signed leaf (TOFU) into <ca_out> (PEM, 0600) and its DER SHA-256 into
# <fp_out>, retrying until SECONDS reaches <deadline_epoch>. Fail on timeout.
_agent_tofu() {
  local host="$1" port="$2" ca="$3" fp="$4" deadline="$5"

  # Clear both first. A capture attempt against a gateway that is not listening
  # yet fails WITHOUT truncating the output file, so a CA left behind by an
  # earlier run would satisfy the non-empty check below: TOFU would report
  # success, the fingerprint would be computed from that stale certificate, and
  # every pinned request for the rest of the run would fail against a
  # certificate this VM never presented. Observed exactly that -- a 12-hour-old
  # ca.pem next to a fresh fingerprint, and a gate that failed with "/readyz
  # never returned 200" while the appliance was perfectly healthy.
  #
  # The subtler hazard is the opposite outcome: had the stale certificate
  # happened to match, TOFU would have "pinned" something it never captured,
  # quietly passing the very property it exists to prove.
  rm -f "$ca" "$fp"
  local tmp="$ca.capture"
  while [ "$SECONDS" -lt "$deadline" ]; do
    rm -f "$tmp"
    if openssl s_client -connect "$host:$port" -showcerts </dev/null 2>/dev/null \
         | openssl x509 -out "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
      mv -f "$tmp" "$ca"
      chmod 600 "$ca" 2>/dev/null || true
      openssl x509 -in "$ca" -outform DER | sha256sum | awk '{print $1}' > "$fp"
      return 0
    fi
    sleep 2
  done
  rm -f "$tmp"
  return 1
}

# _agent_wait_ready <host> <port> <ca> <deadline_epoch> [pid] -- poll /readyz
# through the pinned CA until HTTP 200 or the deadline; if <pid> is given, fail
# fast when that process dies.
_agent_wait_ready() {
  local host="$1" port="$2" ca="$3" deadline="$4" pid="${5:-}"
  local last_rc="" last_status="" last_err=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
      err "QEMU exited before /readyz became ready"; return 1
    fi
    # Capture WHY each attempt failed, not just that it did. "never returned
    # 200" is the same message for a TLS mismatch, a refused connection and a
    # gateway that is simply still starting, and those need different fixes --
    # a self-signed cert regenerated by a gateway restart, for instance, makes
    # every later pinned request fail long after the guest is healthy.
    last_err="$(curl --silent --show-error --cacert "$ca" \
      --write-out '%{http_code}' --output /dev/null \
      "https://$host:$port/readyz" 2>&1)" && last_rc=0 || last_rc=$?
    last_status="${last_err##*$'\n'}"
    if [ "$last_rc" = "0" ] && [ "$last_status" = "200" ]; then
      return 0
    fi
    sleep 3
  done
  err "/readyz never returned 200 (last curl exit=$last_rc, http=$last_status)"
  [ -n "$last_err" ] && err "  last curl output: ${last_err//$'\n'/ }"
  return 1
}

# _agent_wait_down <host> <port> <ca> <deadline_epoch> -- return 0 as soon as
# /readyz stops answering 200 (the guest is going down for a reboot). Best
# effort: return 1 if the deadline passes while it is still ready.
_agent_wait_down() {
  local host="$1" port="$2" ca="$3" deadline="$4"
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! curl --fail --silent --cacert "$ca" \
         "https://$host:$port/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# cmd_contract [IMAGE] -- NON-DESTRUCTIVE live gate. Build a LIVE provisioning
# ISO (hadron_agent digests embedded, NO install block) from IMAGE, boot it via
# direct-kernel in kairos.boot_live_mode against a BLANK qcow2, prove the blank
# disk is never written (sha256 before/after AND QMP write-op count == 0), then
# TOFU the leaf, emit the descriptor, and run the mcp-smoke contract suite with
# the known user + admin bearers.
cmd_contract() {
  shift  # drop "contract"
  local image="${1:-${AGENT_IMAGE:-agent-desktop:dev}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"

  _agent_require_bins qemu-system-x86_64 qemu-img openssl curl python3 docker sha256sum || return 1

  local runtime="${CONTRACT_RUNTIME:-$SCRIPT_DIR/runtime/contract}"
  local art="${CONTRACT_ART:-$SCRIPT_DIR/artifacts/contract}"
  mkdir -p "$runtime" "$art"; chmod 700 "$runtime" "$art" 2>/dev/null || true

  # 1. mint creds + a LIVE cloud-config (no install block): the digests
  #    authorize the live agent, and nothing authorizes a disk wipe.
  local seed_dir="$runtime/seed" seed_out="$runtime/make-seed.env"
  finfo "Minting contract credentials + live cloud-config"
  "$SCRIPT_DIR/make-seed.sh" "$seed_dir" --mode live --listen 0.0.0.0:7443 \
      >"$seed_out" 2>"$art/make-seed.log" \
    || { err "make-seed.sh failed (see $art/make-seed.log)"; return 1; }
  local user_token admin_token user_data
  user_token="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  admin_token="$(sed -n 's/^ADMIN_TOKEN_FILE=//p' "$seed_out")"
  user_data="$(sed -n 's/^USER_DATA=//p' "$seed_out")"
  [ -f "$user_token" ] && [ -f "$admin_token" ] && [ -f "$user_data" ] \
    || { err "make-seed did not produce the expected token/user-data files"; return 1; }

  # 2. build the LIVE provisioning ISO and extract kernel/initrd.
  finfo "Building live provisioning ISO from $image"
  local iso; iso="$(_agent_build_prov_iso "$user_data" "$runtime/iso" "$image")" || return 1
  finfo "Live ISO: $iso"
  local kdir="$runtime/kboot"
  _agent_extract_kernel_initrd "$iso" "$kdir" || return 1

  # 3. blank disk; hash BEFORE.
  local blank="$runtime/blank.qcow2"
  qemu-img create -f qcow2 "$blank" "${CONTRACT_DISK_SIZE:-8G}" >/dev/null \
    || { err "blank disk create failed"; return 1; }
  local hash_before; hash_before="$(_agent_sha256 "$blank")"

  # 4. firmware / accel / loopback wiring.
  local mcp_port vnc_display vnc_port
  mcp_port="$(hdn_agent_alloc_port)"
  vnc_display="${CONTRACT_VNC:-27}"; vnc_port=$((5900 + vnc_display))
  local ovmf_vars="$runtime/OVMF_VARS.fd" qmp="$runtime/qmp.sock"
  local serial="$art/serial.log" qemu_log="$art/qemu.log"
  _agent_set_accel
  _agent_find_ovmf "$ovmf_vars" || return 1
  rm -f "$qmp" "$serial"; touch "$serial"

  # Direct-kernel live boot: the ISO is attached as a READ-ONLY virtio-blk disk
  # (the initrd finds root by CDLABEL=COS_LIVE), the blank disk is the only
  # writable device, and kairos.boot_live_mode boots the appliance
  # NON-DESTRUCTIVELY. NO bootindex here: it conflicts with the direct-kernel
  # path (UEFI-CD install uses bootindex; direct-kernel live must not).
  local cmdline="cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs rd.live.overlay.overlayfs net.ifnames=1 console=tty1 console=ttyS0 kairos.boot_live_mode selinux=0 systemd.mask=serial-getty@ttyS0.service"

  finfo "Booting live agent (MCP 127.0.0.1:$mcp_port -> :7443, VNC 127.0.0.1:$vnc_port)"
  qemu-system-x86_64 "${AGENT_ACCEL[@]}" -m "${MEM:-4096}" -smp "${CPUS:-4}" \
    "${AGENT_OVMF_ARGS[@]}" \
    -device virtio-vga -vnc "127.0.0.1:$vnc_display" \
    -serial "file:$serial" -qmp "unix:$qmp,server,nowait" -rtc base=utc,clock=rt \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${mcp_port}-:7443" \
    -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=blankdisk,format=qcow2,file=$blank" \
    -device "virtio-blk-pci,drive=blankdisk,id=blankdev,serial=contractblank" \
    -drive "if=none,id=livecd,format=raw,readonly=on,file=$iso" \
    -device "virtio-blk-pci,drive=livecd,serial=coslive" \
    -kernel "$kdir/kernel" -initrd "$kdir/initrd" -append "$cmdline" \
    >"$qemu_log" 2>&1 &
  local qemu_pid=$!
  hdn_agent_track_pid "$qemu_pid"

  # 5. TOFU + wait for /readyz.
  local timeout="${CONTRACT_BOOT_TIMEOUT:-600}"
  local deadline=$((SECONDS + timeout))
  local ca="$art/ca.pem" fp="$art/tls-fingerprint.txt"
  finfo "Capturing the self-signed leaf (TOFU)"
  _agent_tofu 127.0.0.1 "$mcp_port" "$ca" "$fp" "$deadline" \
    || { err "could not capture a leaf certificate"; _agent_screenshot "$qmp" "$art/fail.ppm"; return 1; }
  finfo "Waiting for /readyz 200 through the pinned CA"
  _agent_wait_ready 127.0.0.1 "$mcp_port" "$ca" "$deadline" "$qemu_pid" \
    || { err "gateway /readyz never returned 200"; _agent_screenshot "$qmp" "$art/fail.ppm"; return 1; }
  _agent_screenshot "$qmp" "$art/live-ready.ppm"

  # 6. non-destructive gate: the blank disk must be byte-identical AND show no
  #    guest write ops. The hash is the authoritative proof; the QMP write-op
  #    count is a corroborating signal (skipped, with a warning, only if the
  #    device cannot be matched in query-blockstats).
  local hash_after; hash_after="$(_agent_sha256 "$blank")"
  if [ "$hash_before" != "$hash_after" ]; then
    err "RESULT: FAIL - the blank disk hash changed; the live boot wrote to it"
    _agent_screenshot "$qmp" "$art/fail.ppm"; return 1
  fi
  local writes
  if writes="$(qmp_block_writes "$qmp" blankdev 2>/dev/null)"; then
    if [ "${writes:-1}" != "0" ]; then
      err "RESULT: FAIL - $writes guest write op(s) recorded against the blank disk"
      _agent_screenshot "$qmp" "$art/fail.ppm"; return 1
    fi
    finfo "Blank disk unchanged (hash stable, 0 QMP write ops) -- boot is non-destructive"
  else
    err "WARNING: could not match the blank disk in query-blockstats; relying on the hash gate"
    finfo "Blank disk unchanged (hash stable) -- boot is non-destructive"
  fi

  # 7. descriptor + mcp-smoke contract suite (known user + admin bearers).
  local descriptor="$art/contract.json" fingerprint
  fingerprint="$(tr -d '[:space:]' < "$fp")"
  hdn_fixture_emit_descriptor "$descriptor" \
    "https://127.0.0.1:$mcp_port/mcp" "$user_token" "$ca" "$fingerprint" \
    "127.0.0.1:$vnc_port" "" "$qmp" "$art" \
    || { err "descriptor emission failed"; return 1; }

  local smoke; smoke="$(_agent_build_mcp_smoke "$runtime/mcp-smoke")" || return 1
  finfo "Running mcp-smoke contract suite"
  local rc=0
  "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" || rc=$?
  _agent_screenshot "$qmp" "$art/contract-done.ppm"

  # 8. graceful powerdown, then a belt-and-suspenders final hash check.
  [ -S "$qmp" ] && qmp_system_powerdown "$qmp" 2>/dev/null || true
  local hash_final; hash_final="$(_agent_sha256 "$blank")"
  [ "$hash_before" = "$hash_final" ] \
    || { err "RESULT: FAIL - the blank disk changed by the end of the run"; return 1; }

  if [ "$rc" -eq 0 ]; then
    finfo "RESULT: PASS - non-destructive live boot + mcp-smoke contract all green"
  else
    err "RESULT: FAIL - mcp-smoke contract exited $rc"
  fi
  return "$rc"
}

# cmd_install [IMAGE] -- ZERO-TOUCH install gate. Build an INSTALL provisioning
# ISO from IMAGE (install.auto + device + reboot + digests embedded), boot its
# DEFAULT entry via UEFI CD against a blank disk (disk bootindex=0, CD
# bootindex=1), wait through the auto-install + automatic reboot until the
# INSTALLED gateway answers /readyz, run the mcp-smoke contract suite, then do a
# persistence round-trip: write a marker + reboot with the admin bearer, wait
# for the port to drop and return, and read the marker back on the second boot.
cmd_install() {
  shift  # drop "install"
  local image="${1:-${AGENT_IMAGE:-agent-desktop:dev}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"

  _agent_require_bins qemu-system-x86_64 qemu-img openssl curl python3 docker sha256sum || return 1

  local runtime="${INSTALL_RUNTIME:-$SCRIPT_DIR/runtime/install}"
  local art="${INSTALL_ART:-$SCRIPT_DIR/artifacts/install}"
  mkdir -p "$runtime" "$art"; chmod 700 "$runtime" "$art" 2>/dev/null || true

  # 1. mint creds + an INSTALL cloud-config. Only install.{auto,device,reboot}
  #    and the hadron_agent digests are needed: hadron-agent-install injects the
  #    required install.nousers:true into its own generated /oem config.
  local seed_dir="$runtime/seed" seed_out="$runtime/make-seed.env"
  finfo "Minting install credentials + install cloud-config"
  "$SCRIPT_DIR/make-seed.sh" "$seed_dir" --mode install --device /dev/vda --listen 0.0.0.0:7443 \
      >"$seed_out" 2>"$art/make-seed.log" \
    || { err "make-seed.sh failed (see $art/make-seed.log)"; return 1; }
  local user_token admin_token user_data
  user_token="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  admin_token="$(sed -n 's/^ADMIN_TOKEN_FILE=//p' "$seed_out")"
  user_data="$(sed -n 's/^USER_DATA=//p' "$seed_out")"
  [ -f "$user_token" ] && [ -f "$admin_token" ] && [ -f "$user_data" ] \
    || { err "make-seed did not produce the expected token/user-data files"; return 1; }

  # 2. build the INSTALL provisioning ISO.
  finfo "Building install provisioning ISO from $image"
  local iso; iso="$(_agent_build_prov_iso "$user_data" "$runtime/iso" "$image")" || return 1
  finfo "Install ISO: $iso"

  # 3. blank target disk (large enough for a full install).
  local target="$runtime/disk.qcow2"
  qemu-img create -f qcow2 "$target" "${INSTALL_DISK_SIZE:-24G}" >/dev/null \
    || { err "target disk create failed"; return 1; }

  # 4. firmware / accel / loopback wiring.
  local mcp_port vnc_display vnc_port
  mcp_port="$(hdn_agent_alloc_port)"
  vnc_display="${INSTALL_VNC:-28}"; vnc_port=$((5900 + vnc_display))
  local ovmf_vars="$runtime/OVMF_VARS.fd" qmp="$runtime/qmp.sock"
  local serial="$art/serial.log" qemu_log="$art/qemu.log"
  _agent_set_accel
  _agent_find_ovmf "$ovmf_vars" || return 1
  rm -f "$qmp" "$serial"; touch "$serial"

  # UEFI-CD boot of the DEFAULT entry. UEFI honours bootindex: the empty disk
  # (0) is skipped so the CD (1) boots and installs; once the disk is bootable
  # it wins, so the post-install reboot lands in the INSTALLED appliance. The
  # config authorizes hadron-agent-install's zero-touch path to /dev/vda.
  finfo "Booting install ISO default entry (MCP 127.0.0.1:$mcp_port -> :7443, VNC 127.0.0.1:$vnc_port)"
  qemu-system-x86_64 "${AGENT_ACCEL[@]}" -m "${MEM:-4096}" -smp "${CPUS:-4}" \
    "${AGENT_OVMF_ARGS[@]}" \
    -device virtio-vga -vnc "127.0.0.1:$vnc_display" \
    -serial "file:$serial" -qmp "unix:$qmp,server,nowait" -rtc base=utc,clock=rt \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${mcp_port}-:7443" \
    -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=disk,format=qcow2,file=$target" \
    -device "virtio-blk-pci,drive=disk,serial=target,bootindex=0" \
    -drive "if=none,id=cd0,media=cdrom,readonly=on,file=$iso" \
    -device "ide-cd,drive=cd0,bootindex=1" \
    >"$qemu_log" 2>&1 &
  local qemu_pid=$!
  hdn_agent_track_pid "$qemu_pid"
  sleep 3; _agent_screenshot "$qmp" "$art/before-install.ppm"

  # 5. wait through install -> auto reboot -> installed gateway ready. TOFU can
  #    only succeed once the INSTALLED gateway is up, so capture then wait.
  local timeout="${INSTALL_BOOT_TIMEOUT:-1200}"
  local deadline=$((SECONDS + timeout))
  local ca="$art/ca.pem" fp="$art/tls-fingerprint.txt"
  finfo "Waiting for the install + automatic reboot to bring up the installed gateway"
  _agent_tofu 127.0.0.1 "$mcp_port" "$ca" "$fp" "$deadline" \
    || { err "installed gateway leaf never appeared (install may have failed)"; _agent_screenshot "$qmp" "$art/fail.ppm"; return 1; }
  _agent_wait_ready 127.0.0.1 "$mcp_port" "$ca" "$deadline" "$qemu_pid" \
    || { err "installed gateway /readyz never returned 200"; _agent_screenshot "$qmp" "$art/fail.ppm"; return 1; }
  local qstatus; qstatus="$(qmp_query_status "$qmp" 2>/dev/null || echo unknown)"
  finfo "Installed gateway ready (guest status: $qstatus)"
  _agent_screenshot "$qmp" "$art/installed-ready.ppm"

  # 6. descriptor + mcp-smoke contract suite (all seven tools + admin path).
  local descriptor="$art/install.json" fingerprint
  fingerprint="$(tr -d '[:space:]' < "$fp")"
  hdn_fixture_emit_descriptor "$descriptor" \
    "https://127.0.0.1:$mcp_port/mcp" "$user_token" "$ca" "$fingerprint" \
    "127.0.0.1:$vnc_port" "" "$qmp" "$art" \
    || { err "descriptor emission failed"; return 1; }

  local smoke; smoke="$(_agent_build_mcp_smoke "$runtime/mcp-smoke")" || return 1
  finfo "Running mcp-smoke contract suite on the installed appliance"
  local rc=0
  "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" || rc=$?
  if [ "$rc" -ne 0 ]; then
    err "RESULT: FAIL - mcp-smoke contract exited $rc on the installed appliance"
    _agent_screenshot "$qmp" "$art/fail.ppm"; return "$rc"
  fi

  # 7. persistence round-trip. persist-write writes /home/agent/e2e/
  #    persistence-marker and reboots via the admin bearer; we then watch the
  #    forwarded port drop and return, and persist-verify reads the marker back
  #    and reconfirms admin-root + the locked agent account after the reboot.
  local marker
  marker="persist-${RANDOM}-$(date +%s)"
  finfo "Persistence: writing the marker and requesting systemctl reboot (admin bearer)"
  "$smoke" --mode persist-write --descriptor "$descriptor" \
    --admin-bearer-file "$admin_token" --marker "$marker" || {
      err "RESULT: FAIL - persist-write (marker + reboot request) failed"; return 1; }

  local reboot_deadline=$((SECONDS + 180))
  _agent_wait_down 127.0.0.1 "$mcp_port" "$ca" "$reboot_deadline" \
    && finfo "Observed the gateway going down for reboot" \
    || err "WARNING: never observed the gateway drop (reboot may have been very fast)"
  local deadline2=$((SECONDS + ${INSTALL_REBOOT_TIMEOUT:-600}))
  finfo "Waiting for the second boot to return /readyz"
  _agent_wait_ready 127.0.0.1 "$mcp_port" "$ca" "$deadline2" "$qemu_pid" \
    || { err "RESULT: FAIL - installed gateway did not come back after reboot"; _agent_screenshot "$qmp" "$art/fail.ppm"; return 1; }
  _agent_screenshot "$qmp" "$art/after-reboot.ppm"

  finfo "Persistence: verifying the marker + identity survived the reboot"
  "$smoke" --mode persist-verify --descriptor "$descriptor" \
    --admin-bearer-file "$admin_token" --marker "$marker" || {
      err "RESULT: FAIL - persist-verify (marker/identity persistence) failed"; return 1; }

  # 8. graceful powerdown.
  [ -S "$qmp" ] && qmp_system_powerdown "$qmp" 2>/dev/null || true
  finfo "RESULT: PASS - zero-touch install, contract suite, and persistence all green"
  return 0
}

# cmd_generic [ISO] -- lightweight NON-DESTRUCTIVE check of a PLAIN desktop ISO
# (no seed, no cloud-config). Boot it live against a blank disk and require: no
# write to the blank disk (hash + QMP write ops) and NO zero-touch install
# marker in the serial log. There is no agent gateway on a plain desktop image,
# so readiness is not asserted; a screendump is captured for the operator to
# confirm the interactive live desktop.
cmd_generic() {
  shift  # drop "generic"
  local iso="${1:-${GENERIC_ISO:-}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"

  _agent_require_bins qemu-system-x86_64 qemu-img python3 docker sha256sum || return 1

  if [ -z "$iso" ]; then
    iso="$(ls -t "$REPO_ROOT"/build/i3-desktop/iso/*.iso "$REPO_ROOT"/build/sway-desktop/iso/*.iso 2>/dev/null | head -1 || true)"
  fi
  [ -n "$iso" ] && [ -f "$iso" ] \
    || { err "no generic desktop ISO found; pass one: $0 generic path/to.iso (or build with 'make iso')"; return 1; }

  local runtime="${GENERIC_RUNTIME:-$SCRIPT_DIR/runtime/generic}"
  local art="${GENERIC_ART:-$SCRIPT_DIR/artifacts/generic}"
  mkdir -p "$runtime" "$art"; chmod 700 "$runtime" "$art" 2>/dev/null || true

  local kdir="$runtime/kboot"
  _agent_extract_kernel_initrd "$iso" "$kdir" || return 1

  local blank="$runtime/blank.qcow2"
  qemu-img create -f qcow2 "$blank" "${GENERIC_DISK_SIZE:-8G}" >/dev/null \
    || { err "blank disk create failed"; return 1; }
  local hash_before; hash_before="$(_agent_sha256 "$blank")"

  local vnc_display vnc_port ovmf_vars qmp serial qemu_log
  vnc_display="${GENERIC_VNC:-29}"; vnc_port=$((5900 + vnc_display))
  ovmf_vars="$runtime/OVMF_VARS.fd"; qmp="$runtime/qmp.sock"
  serial="$art/serial.log"; qemu_log="$art/qemu.log"
  _agent_set_accel
  _agent_find_ovmf "$ovmf_vars" || return 1
  rm -f "$qmp" "$serial"; touch "$serial"

  local cmdline="cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs rd.live.overlay.overlayfs net.ifnames=1 console=tty1 console=ttyS0 kairos.boot_live_mode selinux=0 systemd.mask=serial-getty@ttyS0.service"

  finfo "Booting plain desktop ISO live (VNC 127.0.0.1:$vnc_port)"
  qemu-system-x86_64 "${AGENT_ACCEL[@]}" -m "${MEM:-4096}" -smp "${CPUS:-4}" \
    "${AGENT_OVMF_ARGS[@]}" \
    -device virtio-vga -vnc "127.0.0.1:$vnc_display" \
    -serial "file:$serial" -qmp "unix:$qmp,server,nowait" -rtc base=utc,clock=rt \
    -netdev user,id=net0 -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=blankdisk,format=qcow2,file=$blank" \
    -device "virtio-blk-pci,drive=blankdisk,id=blankdev,serial=genericblank" \
    -drive "if=none,id=livecd,format=raw,readonly=on,file=$iso" \
    -device "virtio-blk-pci,drive=livecd,serial=coslive" \
    -kernel "$kdir/kernel" -initrd "$kdir/initrd" -append "$cmdline" \
    >"$qemu_log" 2>&1 &
  local qemu_pid=$!
  hdn_agent_track_pid "$qemu_pid"

  # Observe for a bounded window: the plain image must never start a zero-touch
  # install, and the blank disk must stay pristine.
  local observe="${GENERIC_OBSERVE:-180}"
  local deadline=$((SECONDS + observe))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$qemu_pid" 2>/dev/null || { err "QEMU exited during the generic live observation window"; break; }
    if grep -qaiE 'Installing Kairos|kairos-agent install|Zero-touch|authorizes zero-touch|Deploying' "$serial" 2>/dev/null; then
      err "RESULT: FAIL - the plain desktop ISO started a disk install in live mode"
      _agent_screenshot "$qmp" "$art/fail.ppm"; return 1
    fi
    sleep 5
  done
  _agent_screenshot "$qmp" "$art/generic-live.ppm"

  local hash_after; hash_after="$(_agent_sha256 "$blank")"
  if [ "$hash_before" != "$hash_after" ]; then
    err "RESULT: FAIL - the blank disk changed under the plain live desktop"
    return 1
  fi
  local writes
  if writes="$(qmp_block_writes "$qmp" blankdev 2>/dev/null)" && [ "${writes:-1}" != "0" ]; then
    err "RESULT: FAIL - $writes guest write op(s) to the blank disk under the plain live desktop"
    return 1
  fi
  [ -S "$qmp" ] && qmp_system_powerdown "$qmp" 2>/dev/null || true
  finfo "RESULT: PASS - plain desktop booted live, non-destructive, no install started"
  return 0
}

# cmd_fixture [IMAGE] -- boot a long-lived, VNC-visible appliance VM whose MCP
# gateway is forwarded to a loopback host port, TOFU-capture its self-signed
# leaf, verify /readyz through the pinned CA, emit the descriptor, print ONLY
# the descriptor path, and stay running until `stop` powers the guest down.
cmd_fixture() {
  shift  # drop "fixture"
  local image="${1:-${AGENT_IMAGE:-agent-desktop:dev}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"
  _fixture_paths

  # The per-VM credentials MUST be embedded via AuroraBoot --cloud-config
  # (which lands at /run/initramfs/live/config.yaml), so the fixture builds a
  # fresh provisioning ISO from the agent IMAGE each boot rather than reusing a
  # static appliance ISO -- a cidata seed lands at /oem/95_userdata and is NOT
  # read, so it never auto-installs. docker + qemu-img are needed for the build.
  _agent_require_bins qemu-system-x86_64 qemu-img openssl curl python3 docker || return 1

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

  # --- mint per-VM credentials + build the provisioning ISO (Task 2/5) ------
  # make-seed.sh stdout is KEY=VALUE and carries NO bearer plaintext; still
  # route it through redaction as defense in depth. We consume USER_TOKEN_FILE
  # and USER_DATA (the cloud-config we embed via --cloud-config).
  local seed_dir="$FIXTURE_RUNTIME/seed" seed_out="$FIXTURE_RUNTIME/make-seed.env"
  finfo "Minting fixture credentials + install cloud-config"
  if ! "$SCRIPT_DIR/make-seed.sh" "$seed_dir" \
        --mode install --device /dev/vda --listen 0.0.0.0:7443 \
        >"$seed_out" 2>"$FIXTURE_ART/make-seed.log"; then
    err "make-seed.sh failed (see $FIXTURE_ART/make-seed.log)"
    return 1
  fi
  local user_token_file user_data
  user_token_file="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  user_data="$(sed -n 's/^USER_DATA=//p' "$seed_out")"
  [ -n "$user_token_file" ] && [ -f "$user_token_file" ] || { err "make-seed produced no USER_TOKEN_FILE"; return 1; }
  [ -n "$user_data" ] && [ -f "$user_data" ] || { err "make-seed produced no USER_DATA"; return 1; }

  # Build a fresh provisioning ISO from the agent image with the per-VM config
  # embedded, or reuse a prebuilt one via FIXTURE_PROV_ISO.
  local iso="${FIXTURE_PROV_ISO:-}"
  if [ -n "$iso" ]; then
    [ -f "$iso" ] || { err "FIXTURE_PROV_ISO set but not a file: $iso"; return 1; }
    # A prebuilt (released) ISO carries no cloud-config, so this VM's freshly
    # minted bearer digests would never reach the guest: it would boot, report
    # ready, and then reject the descriptor's own token. Remaster a per-run copy
    # with this run's config instead of handing back an ISO that cannot be
    # authenticated against.
    finfo "Injecting this run's cloud-config into the prebuilt ISO"
    local injected="$FIXTURE_RUNTIME/iso/provisioning.iso"
    _agent_inject_cloud_config "$iso" "$user_data" "$injected" || return 1
    iso="$injected"
    finfo "Using prebuilt provisioning ISO (with credentials injected): $iso"
  else
    finfo "Building provisioning ISO from $image"
    iso="$(_agent_build_prov_iso "$user_data" "$FIXTURE_RUNTIME/iso" "$image")" || return 1
    finfo "Provisioning ISO: $iso"
  fi

  # --- launch QEMU via tools/vm.sh with the detachable-fixture knobs --------
  # tools/vm.sh execs qemu, so the backgrounded shell's PID becomes qemu's PID.
  # NO SEED_ISO: the install config is embedded in the ISO via --cloud-config,
  # and tools/vm.sh install boots the DEFAULT entry (disk bootindex=0 skipped
  # while blank, CD bootindex=1 installs, then the disk wins on reboot).
  rm -f "$FIXTURE_QEMU_PID" "$FIXTURE_QMP_SOCK"
  finfo "Booting fixture VM (MCP 127.0.0.1:$mcp_port -> guest :7443, VNC 127.0.0.1:$vnc_port)"
  HOST_MCP_PORT="$mcp_port" \
  QMP="$FIXTURE_QMP_SOCK" \
  SERIAL_LOG="$serial_log" \
  PID_FILE="$FIXTURE_QEMU_PID" \
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
# ---------------------------------------------------------------------------
# Recovery gate (Phase 4 Task 6)
# ---------------------------------------------------------------------------
#
# Every failure below is injected THROUGH THE PUBLIC MCP SURFACE (mcp-smoke
# --mode exec) or through the console (QMP sendkey), never over SSH or a back
# door, so the gate exercises the same path a real operator or client takes.
#
# The hard rule the plan sets: a scenario may only pass if the harness actually
# OBSERVED a degraded/not-ready transition. A service that never went down, or
# went down and came back between two polls, must fail rather than silently
# report success -- otherwise a broken watchdog looks identical to a healthy one.

# _rec_health <ca> <port> -- the redacted /healthz document, or empty when the
# gateway is not answering at all.
_rec_health() {
  curl --fail --silent --max-time 5 --cacert "$1" "https://127.0.0.1:$2/healthz" 2>/dev/null || true
}

# _rec_flag <json> <field> -- one boolean from /healthz as true/false/unknown.
_rec_flag() {
  python3 - "$1" "$2" <<'PY'
import json, sys
raw, field = sys.argv[1], sys.argv[2]
try:
    doc = json.loads(raw)
except Exception:
    print("unknown"); sys.exit(0)
print(str(doc.get(field, "unknown")).lower())
PY
}

# _rec_ready <ca> <port> -- 0 when /readyz answers 200.
_rec_ready() {
  curl --fail --silent --max-time 5 --cacert "$1" "https://127.0.0.1:$2/readyz" >/dev/null 2>&1
}

# _rec_note <art> <line> -- append to the health timeline artifact.
_rec_note() {
  printf '%s %s\n' "$(date -u +%H:%M:%S)" "$2" >> "$1/timeline.log"
  finfo "  $2"
}

# _rec_wait_degraded <ca> <port> <deadline> <art> -- return 0 as soon as the
# appliance reports itself NOT ready, or any health flag flips false. This is
# the observation the plan requires: without it a scenario cannot pass.
_rec_wait_degraded() {
  local ca="$1" port="$2" deadline="$3" art="$4" h
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! _rec_ready "$ca" "$port"; then
      _rec_note "$art" "observed: /readyz stopped answering 200"; return 0
    fi
    h="$(_rec_health "$ca" "$port")"
    if [ -z "$h" ]; then
      _rec_note "$art" "observed: /healthz unreachable"; return 0
    fi
    local f
    for f in session cua gateway; do
      if [ "$(_rec_flag "$h" "$f")" = "false" ]; then
        _rec_note "$art" "observed: healthz.$f went false"; return 0
      fi
    done
    if [ "$(_rec_flag "$h" paused)" = "true" ]; then
      _rec_note "$art" "observed: healthz.paused went true"; return 0
    fi
    sleep 1
  done
  return 1
}

# _rec_wait_recovered <ca> <port> <deadline> <art>
_rec_wait_recovered() {
  local ca="$1" port="$2" deadline="$3" art="$4"
  while [ "$SECONDS" -lt "$deadline" ]; do
    if _rec_ready "$ca" "$port"; then
      _rec_note "$art" "observed: /readyz is 200 again"; return 0
    fi
    sleep 2
  done
  return 1
}

# _rec_exec <smoke> <descriptor> <admin_token> <admin?> <command> -- run one
# command through the public terminal tool. Prints its stdout; returns the
# command's exit status (or the client's reserved code on a transport failure).
_rec_exec() {
  local smoke="$1" descriptor="$2" admin_token="$3" as_admin="$4" cmd="$5"
  local extra=()
  [ "$as_admin" = "admin" ] && extra+=(--exec-admin)
  "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" \
    --mode exec --command "$cmd" "${extra[@]}" 2>/dev/null
}

# Process probes read /proc directly.
#
# The appliance has NO pgrep, and its `ps` is BusyBox, which supports neither
# `-eo` nor `ax`. Both earlier versions of these probes therefore errored to an
# empty string, and an empty answer is indistinguishable from "the process is
# not there" -- so the gate reported that cua-driver was gone and that i3 had
# exited when it had never successfully looked. /proc is always present and
# needs no tools at all.
_rec_count_cmdline() {  # <marker> -- processes whose cmdline contains marker
  # grep -a on /proc/<pid>/cmdline: the file is NUL-separated, so it is binary
  # to grep and needs -a. No tr, which an earlier version got wrong in a way
  # that silently still "worked".
  printf 'n=0; for p in /proc/[0-9]*; do grep -qa %s "$p/cmdline" 2>/dev/null && n=$((n+1)); done; echo $n' "'$1'"
}
_rec_pid_of_comm() {    # <comm> -- first pid whose comm is exactly <comm>
  printf 'for p in /proc/[0-9]*; do [ "$(cat "$p/comm" 2>/dev/null)" = %s ] && { echo ${p#/proc/}; break; }; done' "'$1'"
}
_rec_count_comm() {     # <case pattern> -- processes whose comm matches
  # The pattern is emitted UNQUOTED so `Xorg|X` is a case alternation. Quoting
  # it made the shell look for a process literally named "Xorg|X", so the count
  # was always zero and the gate concluded no X server was running.
  printf 'n=0; for p in /proc/[0-9]*; do case "$(cat "$p/comm" 2>/dev/null)" in %s) n=$((n+1));; esac; done; echo $n' "$1"
}

# _rec_warm_cua <descriptor> <admin_token> <art> -- force the lazily-started
# computer-use backend to come up, by making one computer_use call. Several
# scenarios need a RUNNING driver to kill or to observe reconnecting.
_rec_warm_cua() {
  local descriptor="$1" admin_token="$2" art="$3"
  local smoke_bin; smoke_bin="$(dirname "$descriptor")/mcp-smoke"
  [ -x "$smoke_bin" ] || return 0
  # `warm` issues a real computer_use call. A terminal command will NOT start
  # Cua -- the first version of this helper ran `exec true` and then reported
  # that cua-driver was not running, which was a statement about the helper.
  if "$smoke_bin" --descriptor "$descriptor" --admin-bearer-file "$admin_token" \
       --mode warm >/dev/null 2>&1; then
    _rec_note "$art" "computer-use backend is up"
  else
    _rec_note "$art" "WARNING: could not warm the computer-use backend"
  fi
}

# cmd_recovery [IMAGE] -- RECOVERY gate. Boot an INSTALLED appliance, then kill
# the Cua driver, the session broker, the gateway and the graphical session in
# turn, and toggle the local emergency pause, requiring an observed degraded
# transition and a bounded recovery for each.
cmd_recovery() {
  shift  # drop "recovery"
  local image="${1:-${AGENT_IMAGE:-agent-desktop:dev}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"

  _agent_require_bins qemu-system-x86_64 qemu-img openssl curl python3 sha256sum || return 1

  local runtime="${RECOVERY_RUNTIME:-$SCRIPT_DIR/runtime/recovery}"
  local art="${RECOVERY_ART:-$SCRIPT_DIR/artifacts/recovery}"
  rm -rf "$art"; mkdir -p "$runtime" "$art"; chmod 700 "$runtime" "$art" 2>/dev/null || true
  : > "$art/timeline.log"

  # 1. An installed appliance to break. Reuse the install gate's disk when it
  #    is there (CI runs install first); otherwise run that gate now, because a
  #    recovery gate that quietly skipped would be worse than a slow one.
  local install_runtime="${INSTALL_RUNTIME:-$SCRIPT_DIR/runtime/install}"
  local src_disk="${RECOVERY_DISK:-$install_runtime/disk.qcow2}"
  if [ ! -f "$src_disk" ]; then
    finfo "No installed disk at $src_disk; running the install gate first"
    "$SCRIPT_DIR/run.sh" install "$image" \
      || { err "install gate failed; cannot run recovery"; return 1; }
    src_disk="$install_runtime/disk.qcow2"
  fi
  [ -f "$src_disk" ] || { err "no installed disk to recover: $src_disk"; return 1; }

  local seed_out="$install_runtime/make-seed.env"
  [ -f "$seed_out" ] || { err "missing $seed_out; re-run '$0 install'"; return 1; }
  local user_token admin_token
  user_token="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  admin_token="$(sed -n 's/^ADMIN_TOKEN_FILE=//p' "$seed_out")"
  [ -f "$user_token" ] && [ -f "$admin_token" ] \
    || { err "install seed tokens are missing; re-run '$0 install'"; return 1; }

  local disk="$runtime/disk.qcow2"
  finfo "Cloning the installed disk (the gate is destructive to its copy only)"
  cp --reflink=auto "$src_disk" "$disk" || { err "disk clone failed"; return 1; }

  # 2. Boot the installed system from disk. No CD: this is the appliance as an
  #    operator runs it.
  local mcp_port vnc_display vnc_port
  mcp_port="$(hdn_agent_alloc_port)"
  vnc_display="${RECOVERY_VNC:-27}"; vnc_port=$((5900 + vnc_display))
  local ovmf_vars="$runtime/OVMF_VARS.fd" qmp="$runtime/qmp.sock"
  local serial="$art/serial.log" qemu_log="$art/qemu.log"
  _agent_set_accel
  _agent_find_ovmf "$ovmf_vars" || return 1
  rm -f "$qmp" "$serial"; touch "$serial"

  finfo "Booting the installed appliance (MCP 127.0.0.1:$mcp_port, VNC 127.0.0.1:$vnc_port)"
  qemu-system-x86_64 "${AGENT_ACCEL[@]}" -m "${MEM:-4096}" -smp "${CPUS:-4}" \
    "${AGENT_OVMF_ARGS[@]}" \
    -device virtio-vga -vnc "127.0.0.1:$vnc_display" \
    -serial "file:$serial" -qmp "unix:$qmp,server,nowait" -rtc base=utc,clock=rt \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${mcp_port}-:7443" \
    -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=disk,format=qcow2,file=$disk" \
    -device "virtio-blk-pci,drive=disk,serial=target,bootindex=0" \
    >"$qemu_log" 2>&1 &
  local qpid=$!
  # shellcheck disable=SC2064
  trap "kill $qpid 2>/dev/null || true" RETURN

  local ca="$runtime/ca.pem" fp="$runtime/fingerprint.txt"
  local boot_deadline=$((SECONDS + ${RECOVERY_BOOT_TIMEOUT:-420}))
  _agent_tofu 127.0.0.1 "$mcp_port" "$ca" "$fp" "$boot_deadline" \
    || { err "never captured the TLS leaf"; return 1; }
  _agent_wait_ready 127.0.0.1 "$mcp_port" "$ca" "$boot_deadline" "$qpid" \
    || { err "installed appliance never became ready"; return 1; }
  finfo "Appliance ready"
  _agent_screenshot "$qmp" "$art/00-ready.ppm"

  local descriptor="$runtime/fixture.json"
  hdn_fixture_emit_descriptor "$descriptor" \
    "https://127.0.0.1:$mcp_port/mcp" "$user_token" "$ca" "$(cat "$fp")" \
    "127.0.0.1:$vnc_port" "" "$qmp" "$art" \
    || { err "descriptor emission failed"; return 1; }
  local smoke; smoke="$(_agent_build_mcp_smoke "$runtime/mcp-smoke")" || return 1

  # 3. Long-lived MCP-owned work, so the emergency pause has something to kill.
  #    Deliberately NOT setsid: it must stay in the broker's process group,
  #    which is exactly what pause tears down.
  local longpid
  longpid="$(_rec_exec "$smoke" "$descriptor" "$admin_token" user \
    'nohup sleep 900 >/dev/null 2>&1 & echo $!' | tr -d '[:space:]')"
  _rec_note "$art" "started MCP-owned long process pid=$longpid"

  local failures=0 scenario_timeout="${RECOVERY_SCENARIO_TIMEOUT:-90}"

  # --- scenario 1: kill the Cua driver ------------------------------------
  finfo "[1/6] kill cua-driver"
  # Cua starts LAZILY, so warm it first or there is no driver to kill.
  _rec_warm_cua "$descriptor" "$admin_token" "$art"
  local drv_before
  drv_before="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
    "$(_rec_pid_of_comm cua-driver)" | tr -d '[:space:]')"
  _rec_note "$art" "cua-driver pid before: ${drv_before:-none}"

  if [ -z "$drv_before" ]; then
    err "[1/6] cua-driver is not running after warm-up; cannot exercise its recovery"
    failures=$((failures+1))
  else
    _rec_exec "$smoke" "$descriptor" "$admin_token" admin 'pkill -KILL -f cua-driver || true' >/dev/null

    # The observation is the DRIVER, not /readyz. The broker learns the child
    # died through its reconnect callback, which only fires once something
    # touches the connection -- with no traffic after the kill, readiness stays
    # 200 and a readiness-based wait times out describing nothing. What must
    # actually happen is that the process goes away.
    local drv_gone=0 ddeadline=$((SECONDS + scenario_timeout))
    while [ "$SECONDS" -lt "$ddeadline" ]; do
      local now
      now="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
        "$(_rec_pid_of_comm cua-driver)" | tr -d '[:space:]')"
      if [ -z "$now" ] || [ "$now" != "$drv_before" ]; then
        drv_gone=1
        _rec_note "$art" "observed: cua-driver died (pid $drv_before -> ${now:-none})"
        break
      fi
      sleep 2
    done

    if [ "$drv_gone" -eq 1 ]; then
      # The shell must keep working while computer-use is down: they are
      # different subsystems and only one of them died.
      if [ "$(_rec_exec "$smoke" "$descriptor" "$admin_token" user 'echo alive' | tr -d '[:space:]')" != "alive" ]; then
        err "[1/6] the shell stopped working when cua-driver died"; failures=$((failures+1))
      fi
      # And computer-use must come back on its own, which is the recovery.
      _rec_warm_cua "$descriptor" "$admin_token" "$art"
      local drv_after
      drv_after="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
        "$(_rec_pid_of_comm cua-driver)" | tr -d '[:space:]')"
      if [ -z "$drv_after" ] || [ "$drv_after" = "$drv_before" ]; then
        err "[1/6] cua-driver did not come back (pid now ${drv_after:-none})"
        failures=$((failures+1))
      else
        _rec_note "$art" "observed: cua-driver reconnected as pid $drv_after"
      fi
    else
      err "[1/6] cua-driver never died; the kill did not take effect"
      failures=$((failures+1))
    fi
  fi
  _agent_screenshot "$qmp" "$art/01-cua-driver.ppm"

  # --- scenario 2: kill the unprivileged session broker --------------------
  finfo "[2/6] kill the session broker"
  _rec_exec "$smoke" "$descriptor" "$admin_token" admin \
    'pkill -KILL -f "hadron-agent session" || true' >/dev/null
  if _rec_wait_degraded "$ca" "$mcp_port" $((SECONDS + scenario_timeout)) "$art"; then
    _rec_wait_recovered "$ca" "$mcp_port" $((SECONDS + scenario_timeout)) "$art" \
      || { err "[2/6] the user systemd unit did not restart the broker in time"; failures=$((failures+1)); }
  else
    err "[2/6] never observed a degraded transition"; failures=$((failures+1))
  fi
  _agent_screenshot "$qmp" "$art/02-session-broker.ppm"

  # --- scenario 3: kill the gateway ---------------------------------------
  # Injected with the admin bearer through the gateway that is about to die, so
  # the call itself may not return; that is expected, not a failure.
  finfo "[3/6] kill the gateway"
  _rec_exec "$smoke" "$descriptor" "$admin_token" admin \
    'systemctl kill --signal=SIGKILL hadron-agent-gateway || true' >/dev/null 2>&1 || true
  if _rec_wait_degraded "$ca" "$mcp_port" $((SECONDS + scenario_timeout)) "$art"; then
    if _rec_wait_recovered "$ca" "$mcp_port" $((SECONDS + scenario_timeout)) "$art"; then
      # The SAME bearer must still be accepted: a restart must not rotate or
      # forget the provisioned digests.
      if [ "$(_rec_exec "$smoke" "$descriptor" "$admin_token" user 'echo back' | tr -d '[:space:]')" != "back" ]; then
        err "[3/6] the pre-restart bearer stopped working"; failures=$((failures+1))
      fi
    else
      err "[3/6] systemd did not restart the gateway in time"; failures=$((failures+1))
    fi
  else
    err "[3/6] never observed the port close"; failures=$((failures+1))
  fi
  _agent_screenshot "$qmp" "$art/03-gateway.ppm"

  # --- scenario 4: exit the graphical session -----------------------------
  finfo "[4/6] i3-msg exit"
  # The observation here is NOT /readyz. Exiting i3 does not make the appliance
  # unready: the gateway and the session broker are separate services that keep
  # answering while the window manager is gone. Waiting for a readiness dip
  # therefore timed out and reported "never observed a degraded transition" --
  # a statement about the wrong signal, not about the watchdog.
  #
  # What must actually happen is that the window manager DIES and the watchdog
  # brings it back on the same seat. So observe i3's pid: it must change, and
  # exactly one X server must remain.
  local i3_before
  i3_before="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
    "$(_rec_pid_of_comm i3)" | tr -d '[:space:]')"
  _rec_note "$art" "i3 pid before: ${i3_before:-none}"
  _rec_exec "$smoke" "$descriptor" "$admin_token" user \
    'i3-msg exit >/dev/null 2>&1 || pkill -KILL -x i3 || true' >/dev/null 2>&1 || true

  local i3_gone=0 sdeadline=$((SECONDS + scenario_timeout))
  while [ "$SECONDS" -lt "$sdeadline" ]; do
    local now
    now="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
      "$(_rec_pid_of_comm i3)" | tr -d '[:space:]')"
    if [ -z "$now" ] || { [ -n "$i3_before" ] && [ "$now" != "$i3_before" ]; }; then
      i3_gone=1; _rec_note "$art" "observed: i3 went away (pid ${i3_before:-none} -> ${now:-none})"; break
    fi
    sleep 2
  done

  if [ "$i3_gone" -eq 1 ]; then
    if _rec_wait_recovered "$ca" "$mcp_port" $((SECONDS + ${RECOVERY_SESSION_TIMEOUT:-150})) "$art"; then
      # Wait for the window manager itself, not just readiness. The next
      # scenario presses an i3 keybinding, so continuing before i3 is back
      # tests nothing and fails for the wrong reason.
      local i3_back="" wdeadline=$((SECONDS + ${RECOVERY_SESSION_TIMEOUT:-150}))
      while [ "$SECONDS" -lt "$wdeadline" ]; do
        i3_back="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
          "$(_rec_pid_of_comm i3)" | tr -d '[:space:]')"
        [ -n "$i3_back" ] && { _rec_note "$art" "observed: i3 is back (pid $i3_back)"; break; }
        sleep 3
      done
      [ -n "$i3_back" ] || { err "[4/6] i3 did not come back"; failures=$((failures+1)); }

      # The watchdog must bring the session back on the REAL seat: exactly one
      # X server, on tty1. A hidden second display would satisfy "ready" while
      # leaving the console dark, which is the failure this guards.
      local displays
      displays="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
        "$(_rec_count_comm 'Xorg|X')" | tr -d '[:space:]')"
      _rec_note "$art" "X servers after recovery: ${displays:-0}"
      if [ "${displays:-0}" != "1" ]; then
        err "[4/6] expected exactly one X server after recovery, found ${displays:-0}"
        failures=$((failures+1))
      fi
    else
      err "[4/6] the display watchdog did not restore the session in time"; failures=$((failures+1))
    fi
  else
    err "[4/6] i3 never went away; the exit request did not take effect"
    failures=$((failures+1))
  fi
  _agent_screenshot "$qmp" "$art/04-session-restart.ppm"

  # --- scenario 5: local emergency pause ----------------------------------
  # Sent as a real console keystroke (Super+Shift+Escape) over QMP, because the
  # whole point of the chord is that someone physically at the machine can halt
  # remote control WITHOUT using the remote interface.
  finfo "[5/6] emergency pause chord"
  hmp_sendkey "$qmp" meta_l-shift-esc || true
  local paused_seen=0 pdeadline=$((SECONDS + scenario_timeout))
  while [ "$SECONDS" -lt "$pdeadline" ]; do
    if [ "$(_rec_flag "$(_rec_health "$ca" "$mcp_port")" paused)" = "true" ]; then
      paused_seen=1; _rec_note "$art" "observed: healthz.paused is true"; break
    fi
    sleep 1
  done
  if [ "$paused_seen" -eq 1 ]; then
    # Both credential classes must be refused while paused.
    local urc=0 arc=0
    _rec_exec "$smoke" "$descriptor" "$admin_token" user  'echo nope' >/dev/null 2>&1 || urc=$?
    _rec_exec "$smoke" "$descriptor" "$admin_token" admin 'echo nope' >/dev/null 2>&1 || arc=$?
    if [ "$urc" -eq 0 ] || [ "$arc" -eq 0 ]; then
      err "[5/6] a tool call succeeded while paused (user rc=$urc admin rc=$arc)"
      failures=$((failures+1))
    fi
    _agent_screenshot "$qmp" "$art/05-paused.ppm"
  else
    err "[5/6] the pause chord never took effect"; failures=$((failures+1))
  fi

  # --- scenario 6: resume -------------------------------------------------
  finfo "[6/6] emergency resume"
  hmp_sendkey "$qmp" meta_l-shift-esc || true
  if _rec_wait_recovered "$ca" "$mcp_port" $((SECONDS + scenario_timeout)) "$art"; then
    if [ "$(_rec_exec "$smoke" "$descriptor" "$admin_token" user 'echo resumed' | tr -d '[:space:]')" != "resumed" ]; then
      err "[6/6] tools did not work again after resume"; failures=$((failures+1))
    fi
    # The paused run must not be replayed: the long process pause killed must
    # stay dead rather than being restarted behind the operator's back.
    if [ -n "$longpid" ]; then
      local still
      still="$(_rec_exec "$smoke" "$descriptor" "$admin_token" admin \
        "kill -0 $longpid 2>/dev/null && echo alive || echo gone" | tr -d '[:space:]')"
      if [ "$still" = "alive" ]; then
        err "[6/6] the MCP-owned process survived the pause (expected it to be torn down)"
        failures=$((failures+1))
      else
        _rec_note "$art" "observed: the MCP-owned process stayed dead after resume (no replay)"
      fi
    fi
  else
    err "[6/6] never became ready again after resume"; failures=$((failures+1))
  fi
  _agent_screenshot "$qmp" "$art/06-resumed.ppm"

  # 4. Diagnostics, retained whatever the outcome.
  _rec_exec "$smoke" "$descriptor" "$admin_token" admin \
    'journalctl -b --no-pager -n 2000 2>/dev/null || true' > "$art/system-journal.log" 2>/dev/null || true
  _rec_exec "$smoke" "$descriptor" "$admin_token" user \
    'journalctl --user -b --no-pager -n 2000 2>/dev/null || true' > "$art/user-journal.log" 2>/dev/null || true
  _rec_health "$ca" "$mcp_port" > "$art/healthz-final.json" 2>/dev/null || true

  [ -S "$qmp" ] && qmp_system_powerdown "$qmp" 2>/dev/null || true

  if [ "$failures" -eq 0 ]; then
    finfo "RESULT: PASS - all six recovery scenarios observed a failure and recovered"
    return 0
  fi
  err "RESULT: FAIL - $failures recovery scenario(s) failed (see $art)"
  return 1
}

# ---------------------------------------------------------------------------
# UI gate (Phase 4 Task 7)
# ---------------------------------------------------------------------------
#
# Drives a real GTK application and a real Chromium in the appliance's visible
# session using ONLY the public tools, and asserts against each application's
# own state rather than against the harness's expectations.
#
# The Chromium half deliberately does NOT use the `browser` tool. browser is
# CDP-backed, and a gate that asserted through CDP would be asking Chromium to
# grade its own homework: the point here is that the DESKTOP path -- pixels and
# accessibility -- controls a real browser window.

# _ui_exec <smoke> <descriptor> <admin_token> <user|admin> <command>
# _ui_exec <smoke> <descriptor> <admin_token> <user|admin> <command> [timeout]
#
# The timeout matters: installing Chromium from Flathub takes minutes, and the
# client's 30s default cut it off mid-download. stdout carries ONLY the
# command's own output, so a caller can parse it directly.
# _ui_exec <smoke> <descriptor> <admin_token> <user|admin> <command> [stderr_log]
#
# stdout is the command's own output so a caller can parse it. stderr carries
# mcp-smoke's diagnosis and is NOT discarded: swallowing it is how an install
# that failed instantly kept looking like an install that produced no output.
# Pass a log path to keep it; otherwise it flows to the caller's stderr.
_ui_exec() {
  local smoke="$1" descriptor="$2" admin_token="$3" as="$4" cmd="$5" errlog="${6:-}"
  local extra=()
  [ "$as" = "admin" ] && extra+=(--exec-admin)
  if [ -n "$errlog" ]; then
    "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" \
      --mode exec --command "$cmd" "${extra[@]}" 2>>"$errlog"
  else
    "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" \
      --mode exec --command "$cmd" "${extra[@]}"
  fi
}

# cmd_ui [IMAGE] -- UI gate on an installed appliance.
cmd_ui() {
  shift  # drop "ui"
  local image="${1:-${AGENT_IMAGE:-agent-desktop:dev}}"

  # shellcheck source=lib/common.sh
  source "$SCRIPT_DIR/lib/common.sh"
  # shellcheck source=lib/qmp.sh
  source "$SCRIPT_DIR/lib/qmp.sh"
  # shellcheck source=lib/fixture.sh
  source "$SCRIPT_DIR/lib/fixture.sh"

  _agent_require_bins qemu-system-x86_64 qemu-img openssl curl python3 docker sha256sum tar || return 1

  local runtime="${UI_RUNTIME:-$SCRIPT_DIR/runtime/ui}"
  local art="${UI_ART:-$SCRIPT_DIR/artifacts/ui}"
  rm -rf "$art"; mkdir -p "$runtime" "$art"; chmod 700 "$runtime" "$art" 2>/dev/null || true

  # 1. Fixtures, built against the runtime under test.
  local fixture_tar="${UI_FIXTURE_TAR:-$SCRIPT_DIR/runtime/fixtures/ui-fixtures.tar}"
  if [ ! -f "$fixture_tar" ]; then
    finfo "Building UI fixtures"
    AGENT_IMAGE="$image" "$SCRIPT_DIR/build-fixtures.sh" \
      || { err "fixture build failed"; return 1; }
  fi
  [ -f "$fixture_tar" ] && [ -f "$fixture_tar.sha256" ] \
    || { err "missing fixture tar or checksum: $fixture_tar"; return 1; }
  local fixture_sha; fixture_sha="$(cat "$fixture_tar.sha256")"

  # 2. An installed appliance, exactly as the recovery gate obtains one.
  local install_runtime="${INSTALL_RUNTIME:-$SCRIPT_DIR/runtime/install}"
  local src_disk="${UI_DISK:-$install_runtime/disk.qcow2}"
  if [ ! -f "$src_disk" ]; then
    finfo "No installed disk at $src_disk; running the install gate first"
    "$SCRIPT_DIR/run.sh" install "$image" || { err "install gate failed"; return 1; }
    src_disk="$install_runtime/disk.qcow2"
  fi
  local seed_out="$install_runtime/make-seed.env"
  [ -f "$seed_out" ] || { err "missing $seed_out; re-run '$0 install'"; return 1; }
  local user_token admin_token
  user_token="$(sed -n 's/^USER_TOKEN_FILE=//p' "$seed_out")"
  admin_token="$(sed -n 's/^ADMIN_TOKEN_FILE=//p' "$seed_out")"

  local disk="$runtime/disk.qcow2"
  cp --reflink=auto "$src_disk" "$disk" || { err "disk clone failed"; return 1; }

  local mcp_port vnc_display vnc_port
  mcp_port="$(hdn_agent_alloc_port)"
  vnc_display="${UI_VNC:-26}"; vnc_port=$((5900 + vnc_display))
  local ovmf_vars="$runtime/OVMF_VARS.fd" qmp="$runtime/qmp.sock"
  local serial="$art/serial.log" qemu_log="$art/qemu.log"
  _agent_set_accel
  _agent_find_ovmf "$ovmf_vars" || return 1
  rm -f "$qmp" "$serial"; touch "$serial"

  finfo "Booting the installed appliance (MCP 127.0.0.1:$mcp_port, VNC 127.0.0.1:$vnc_port)"
  qemu-system-x86_64 "${AGENT_ACCEL[@]}" -m "${MEM:-6144}" -smp "${CPUS:-4}" \
    "${AGENT_OVMF_ARGS[@]}" \
    -device virtio-vga -vnc "127.0.0.1:$vnc_display" \
    -serial "file:$serial" -qmp "unix:$qmp,server,nowait" -rtc base=utc,clock=rt \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${mcp_port}-:7443" \
    -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=disk,format=qcow2,file=$disk" \
    -device "virtio-blk-pci,drive=disk,serial=target,bootindex=0" \
    >"$qemu_log" 2>&1 &
  local qpid=$!
  # shellcheck disable=SC2064
  trap "kill $qpid 2>/dev/null || true" RETURN

  local ca="$runtime/ca.pem" fp="$runtime/fingerprint.txt"
  local deadline=$((SECONDS + ${UI_BOOT_TIMEOUT:-420}))
  _agent_tofu 127.0.0.1 "$mcp_port" "$ca" "$fp" "$deadline" \
    || { err "never captured the TLS leaf"; return 1; }
  _agent_wait_ready 127.0.0.1 "$mcp_port" "$ca" "$deadline" "$qpid" \
    || { err "appliance never became ready"; return 1; }

  local descriptor="$runtime/fixture.json"
  hdn_fixture_emit_descriptor "$descriptor" \
    "https://127.0.0.1:$mcp_port/mcp" "$user_token" "$ca" "$(cat "$fp")" \
    "127.0.0.1:$vnc_port" "" "$qmp" "$art" \
    || { err "descriptor emission failed"; return 1; }
  local smoke; smoke="$(_agent_build_mcp_smoke "$runtime/mcp-smoke")" || return 1

  # 3. Upload and unpack the fixtures through the public tools.
  finfo "Uploading fixtures via write_file"
  "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" \
    --mode upload --local-file "$fixture_tar" \
    --remote-file /home/agent/e2e/ui-fixtures.tar --sha256 "$fixture_sha" \
    || { err "fixture upload failed"; _agent_screenshot "$qmp" "$art/fail-upload.ppm"; return 1; }

  _ui_exec "$smoke" "$descriptor" "$admin_token" user \
    'set -e; cd /home/agent/e2e; tar -xf ui-fixtures.tar; chmod 0755 bin/hadron-cua-gtk; ls -l bin web' \
    > "$art/fixture-install.log" 2>&1 \
    || { err "unpacking the fixtures failed (see $art/fixture-install.log)"; return 1; }

  # 4. Launch the GTK fixture on the REAL seat. DISPLAY/XAUTHORITY come from
  #    the session the broker already lives in, so this lands on the visible
  #    desktop rather than on a hidden server.
  finfo "Launching the GTK fixture"
  _ui_exec "$smoke" "$descriptor" "$admin_token" user \
    'nohup /home/agent/e2e/bin/hadron-cua-gtk >/home/agent/e2e/gtk.log 2>&1 & sleep 3; echo started' \
    >/dev/null 2>&1 || true

  # 5. Chromium at the pinned commit. The commit is asserted, not assumed: a
  #    Flathub update between runs would otherwise silently change what the
  #    gate tests.
  local want_commit; want_commit="$(cat "$SCRIPT_DIR/fixtures/chromium.commit")"
  finfo "Installing Chromium at the pinned commit ${want_commit:0:12}"
  # bash is unbounded, so the multi-minute Flathub install is just one call --
  # no start/poll handshake, and nothing to lose the process handle to. The
  # installer's own output is kept so a failure explains itself.
  _ui_exec "$smoke" "$descriptor" "$admin_token" user \
    "set -x
     flatpak --version
     flatpak remote-add --user --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo
     flatpak install -y --user --noninteractive flathub org.chromium.Chromium
     flatpak update -y --user --commit=$want_commit org.chromium.Chromium || true
     echo COMMIT_BEGIN
     flatpak info --user --show-commit org.chromium.Chromium
     echo COMMIT_END" \
    > "$art/chromium-install.log" 2>&1 || true
  cp -f "$art/chromium-install.log" "$art/chromium-commit.txt" 2>/dev/null || true

  # Take the LAST line: flatpak prints progress before the commit, and a
  # partial read here would compare noise against the pinned digest.
  local got_commit
  got_commit="$(grep -oE '^[0-9a-f]{64}$' "$art/chromium-install.log" 2>/dev/null | tail -1 || true)"
  if [ "$got_commit" != "$want_commit" ]; then
    err "Chromium commit mismatch: guest has '${got_commit:0:12}', fixtures pin '${want_commit:0:12}'"
    err "  a drifting browser silently changes what this gate proves"
    err "  installer output (last 15 lines):"
    tail -15 "$art/chromium-install.log" 2>/dev/null | sed 's/^/    /' >&2 || true
    _agent_screenshot "$qmp" "$art/fail-chromium-commit.ppm"
    return 1
  fi
  finfo "Chromium commit verified: ${got_commit:0:12}"

  finfo "Launching Chromium on the fixture page"
  _ui_exec "$smoke" "$descriptor" "$admin_token" user \
    'nohup flatpak run --socket=x11 --socket=session-bus \
       --env=DISPLAY="$DISPLAY" --env=XAUTHORITY="$XAUTHORITY" --filesystem="$XAUTHORITY" \
       --filesystem=/home/agent/e2e \
       org.chromium.Chromium --ozone-platform=x11 --no-sandbox --disable-gpu \
       --disable-dev-shm-usage --no-first-run --force-renderer-accessibility \
       file:///home/agent/e2e/web/index.html >/home/agent/e2e/chromium.log 2>&1 &
     sleep 20; echo launched' >/dev/null 2>&1 || true

  _agent_screenshot "$qmp" "$art/10-fixtures-up.ppm"

  # 6. Drive and assert, entirely through the public tools.
  finfo "Running the UI suite"
  local rc=0
  "$smoke" --descriptor "$descriptor" --admin-bearer-file "$admin_token" --mode ui || rc=$?

  # 7. Framebuffer cross-check: what the agent SEES must be what the machine is
  #    really scanning out. A capture that agreed with itself but not with QEMU
  #    would mean the agent is driving something the console never shows.
  local cua_png="$art/cua-desktop.png" qmp_ppm="$art/qmp-desktop.ppm"
  _agent_screenshot "$qmp" "$qmp_ppm"
  if [ -f "$cua_png" ] && [ -f "$qmp_ppm" ]; then
    if python3 "$SCRIPT_DIR/frame_compare.py" "$cua_png" "$qmp_ppm" "$art/frame-compare.json"; then
      finfo "Framebuffer comparison passed"
    else
      err "framebuffer comparison failed (see $art/frame-compare.json)"
      rc=1
    fi
  else
    err "missing capture(s) for the framebuffer comparison; skipping it"
  fi

  # 8. Diagnostics either way; a VNC-visible shot on failure.
  _ui_exec "$smoke" "$descriptor" "$admin_token" user \
    'cat /home/agent/e2e/gtk.log 2>/dev/null; echo ---; cat /home/agent/e2e/chromium.log 2>/dev/null | tail -50' \
    > "$art/fixture-logs.txt" 2>&1 || true
  if [ "$rc" -ne 0 ]; then
    _agent_screenshot "$qmp" "$art/fail.ppm"
  fi

  [ -S "$qmp" ] && qmp_system_powerdown "$qmp" 2>/dev/null || true

  if [ "$rc" -eq 0 ]; then
    finfo "RESULT: PASS - GTK and Chromium driven through the public tools"
    return 0
  fi
  err "RESULT: FAIL - the UI gate exited $rc (see $art)"
  return "$rc"
}

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
  contract) cmd_contract "$@"; exit $? ;;
  install)  cmd_install  "$@"; exit $? ;;
  generic)  cmd_generic  "$@"; exit $? ;;
  recovery) cmd_recovery "$@"; exit $? ;;
  ui)       cmd_ui       "$@"; exit $? ;;
  fixture)  cmd_fixture  "$@"; exit $? ;;
  stop)     cmd_stop     "$@"; exit $? ;;
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
