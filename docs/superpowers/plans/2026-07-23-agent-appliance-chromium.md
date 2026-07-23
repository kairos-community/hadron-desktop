# Agent Appliance First-Boot Chromium Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The agent appliance installs Chromium on first boot and keeps it running with the Chrome DevTools debug port open, so the Cua `browser` tool works on a fresh machine with no human starting a browser.

**Architecture:** A root system oneshot installs `org.chromium.Chromium` system-wide via the signed Flathub remote the image already ships; a cos-setup `rootfs` stage persists `/var/lib/flatpak` so the install survives reboots; a per-user (`agent`) systemd unit launches Chromium with `--remote-debugging-port=9222` and restarts it on crash. Flatpak's own install state is the single source of truth — no separate marker file — so every unit is idempotent and self-healing. Everything is guarded off non-agent images by the `/etc/hadron-agent/profile` marker.

**Tech Stack:** POSIX sh, systemd (system + user units), Flatpak, Kairos cos-setup yip stages, Docker BuildKit.

**Spec:** `docs/superpowers/specs/2026-07-23-agent-appliance-chromium.md`

## Global Constraints

- **Agent-only.** Every unit gates on `ConditionPathExists=/etc/hadron-agent/profile`. The persistence yip stage carries **no `if:` guard** — it runs in the initramfs `rootfs` context where the installed `/etc` is not yet live (a guard silently skips it, the exact bug the `93_agent_persistence.yaml` comment documents); the stage ships only in the agent overlay, so its mere presence is the profile check.
- **New binaries go in `/usr/bin`**, never `/usr/local/bin` — `/usr/local` is shadowed by an empty overlay at runtime on Kairos. (Note the existing `hadron-agent-install` lives in `/usr/local/bin` because it runs only in the live installer environment; these scripts run on the installed system, so `/usr/bin`.)
- **Signed remote only.** Reuse the image's baked descriptor `/usr/share/hadron/flathub.flatpakrepo` (URL `https://dl.flathub.org/repo/`, fingerprint `6E5C05D979C76DAF93C081354184DD4D907A7CAE`). Never add an unsigned remote; never pass `--no-gpg-verify`.
- **Install system-wide** (`flatpak --system`, lands in `/var/lib/flatpak`), run as **root**. Launch runs as the **`agent`** user (UID 1000).
- **Track `stable`, do not pin an OSTree commit** in production (decision D3): a pinned commit can be garbage-collected off Flathub and fail a months-later first boot. The test harness keeps its own pin (`test/agent/fixtures/chromium.commit`); production and test deliberately diverge.
- **Never block boot.** The install unit is a leaf on `multi-user.target`; a network outage means it fails and retries next boot while the appliance stays usable.
- **Flatpak state is the single source of truth.** Gate on `flatpak info --system org.chromium.Chromium`, not a bespoke marker file — so a persisted-but-inconsistent state self-heals.
- **Injectable command seam for tests.** The install script calls flatpak through `${HADRON_FLATPAK:-flatpak}`, mirroring the `HADRON_INSTALL_CMD` pattern in `hadron-agent-install`, so tests assert behaviour with a fake flatpak instead of pulling ~2 GB.
- Launch flags, taken verbatim from the proven test fixture `test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf` and `test/agent/run.sh`: `--ozone-platform=x11 --no-sandbox --disable-gpu --disable-dev-shm-usage --no-first-run --force-renderer-accessibility --remote-debugging-port=9222 --remote-allow-origins=*`.

---

### Task 1: System install unit and script

The root oneshot that installs Chromium on first boot. Independently testable via the injectable flatpak seam.

**Files:**
- Create: `rootfs-agent/usr/bin/hadron-agent-browser-install`
- Create: `rootfs-agent/etc/systemd/system/hadron-agent-browser-install.service`
- Test: `test/agent/browser_install_test.sh`

**Interfaces:**
- Consumes: the baked descriptor `/usr/share/hadron/flathub.flatpakrepo`; `${HADRON_FLATPAK:-flatpak}`.
- Produces: `org.chromium.Chromium` installed system-wide (or, under test, the fake flatpak's recorded actions). Task 3's launch wrapper depends on `flatpak info --system org.chromium.Chromium` succeeding after this runs.

- [ ] **Step 1: Write the failing test**

Create `test/agent/browser_install_test.sh`. It injects a fake flatpak that records its argv and simulates "already installed" or "not installed" via a control file — so the suite proves the three behaviours (skip when present, add-remote-then-install when absent, fail cleanly when install fails) without touching real Flatpak or the network.

```bash
#!/usr/bin/env bash
# Tests for hadron-agent-browser-install: installs org.chromium.Chromium
# system-wide on first boot, idempotently, through the HADRON_FLATPAK seam.
#
# The real flatpak is intercepted by a fake that appends each invocation to
# $CALLS and answers `info` from a control file, so a test can assert exactly
# which flatpak subcommands ran without pulling ~2 GB or hitting the network.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/rootfs-agent/usr/bin/hadron-agent-browser-install"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# 1. syntax
if sh -n "$script" 2>/dev/null; then ok "sh -n"; else err "sh -n failed"; fi

# Build a fake flatpak: `info --system org.chromium.Chromium` exits 0 iff the
# control file $INSTALLED_FLAG exists; everything else records its args and, for
# `install`, creates the flag (so a later `info` reports it present).
make_fake_flatpak() { # $1 = dir to hold the fake + its logs
  d="$1"
  cat > "$d/flatpak" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$d/calls"
case "\$1 \$2" in
  "info --system")
    shift 2
    [ "\$1" = "org.chromium.Chromium" ] && { [ -e "$d/installed" ] && exit 0 || exit 1; } ;;
  "install --system"|"install --noninteractive")
    if [ -e "$d/fail_install" ]; then exit 1; fi
    : > "$d/installed" ;;
esac
exit 0
EOF
  chmod +x "$d/flatpak"
}

# 2. already installed -> no remote-add, no install
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
  sh "$script" >/dev/null 2>&1
if grep -q "install" "$d/calls" 2>/dev/null; then
  err "already-installed run must not call install"
else ok "already installed -> no install"; fi
rm -rf "$d"

# 3. not installed -> adds the remote then installs
d="$(mktemp -d)"; make_fake_flatpak "$d"
HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
  sh "$script" >/dev/null 2>&1
if grep -q "remote-add" "$d/calls" && grep -q "install" "$d/calls"; then
  ok "absent -> remote-add + install"
else err "absent run must add the remote and install (got: $(cat "$d/calls"))"; fi
rm -rf "$d"

# 4. install failure -> script exits non-zero (unit will retry next boot)
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/fail_install"
if HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
     sh "$script" >/dev/null 2>&1; then
  err "a failed install must make the script exit non-zero"
else ok "install failure -> non-zero exit"; fi
rm -rf "$d"

exit "$fail"
```

```bash
chmod +x test/agent/browser_install_test.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/agent/browser_install_test.sh
```

Expected: `FAIL: sh -n failed` (the script does not exist yet) and the later cases fail because there is nothing to run.

- [ ] **Step 3: Write the install script**

Create `rootfs-agent/usr/bin/hadron-agent-browser-install`:

```sh
#!/bin/sh
# Install Chromium system-wide on the agent appliance, idempotently.
#
# Runs as root from hadron-agent-browser-install.service (a first-boot oneshot).
# Flatpak's own install state is the single source of truth: if the app is
# already present (a persisted /var/lib/flatpak — see 94_agent_browser_persistence
# .yaml), this is a fast local no-op needing no network. Otherwise it adds the
# signed Flathub remote the image already ships and installs the stable branch.
#
# We track `stable` rather than pinning an OSTree commit on purpose: the appliance
# may first-boot long after the image was built, and Flathub garbage-collects old
# commits, so a pin can become uninstallable. The test harness keeps its own pin
# (test/agent/fixtures/chromium.commit); production and test deliberately diverge.
#
# Seams (tests only):
#   HADRON_FLATPAK         flatpak binary (default "flatpak"); tests inject a fake.
#   HADRON_PROFILE_MARKER  profile marker path (default /etc/hadron-agent/profile).
set -eu

FLATPAK="${HADRON_FLATPAK:-flatpak}"
MARKER="${HADRON_PROFILE_MARKER:-/etc/hadron-agent/profile}"
REMOTE=flathub
DESCRIPTOR=/usr/share/hadron/flathub.flatpakrepo
APP=org.chromium.Chromium

# Defence in depth: the unit already gates on the profile marker, but re-assert
# it so a manual run on the wrong machine is a clean no-op, not a 2 GB download.
[ -e "$MARKER" ] || { echo "not the agent appliance (no $MARKER); nothing to do"; exit 0; }

# Already installed? Local check, no network.
if "$FLATPAK" info --system "$APP" >/dev/null 2>&1; then
    echo "$APP already installed"
    exit 0
fi

# Add the signed remote if it is not already present. The descriptor carries the
# URL and GPG key; we never pass --no-gpg-verify.
if ! "$FLATPAK" remotes --system --columns=name 2>/dev/null | grep -qx "$REMOTE"; then
    echo "adding signed Flathub remote"
    "$FLATPAK" remote-add --system "$REMOTE" "$DESCRIPTOR"
fi

echo "installing $APP (stable) system-wide"
"$FLATPAK" install --system --noninteractive -y "$REMOTE" "$APP"
echo "installed $APP"
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
test/agent/browser_install_test.sh
```

Expected: four `ok:` lines, exit 0.

- [ ] **Step 5: Write the system unit**

Create `rootfs-agent/etc/systemd/system/hadron-agent-browser-install.service`. Mirrors the ordering conventions of `hadron-agent-provision.service` (profile gate, network-online, cos-setup ordering, `Type=oneshot` + `RemainAfterExit=yes`), and — deliberately — has **no run-once marker**: the script self-no-ops when the app is present, which self-heals a partially-persisted state that a marker file would mask.

```ini
# Install Chromium on the agent appliance so the Cua browser tool has a browser
# to drive over CDP (127.0.0.1:9222). First-boot oneshot; the script is a fast
# local no-op once the app is installed (and /var/lib/flatpak is persisted), so
# re-running every boot costs nothing and self-heals an inconsistent state.
#
# ConditionPathExists=/etc/hadron-agent/profile makes this a hard no-op on any
# non-appliance image (the dangling wants-symlink does nothing there).
[Unit]
Description=Hadron agent Chromium install (system Flatpak)
ConditionPathExists=/etc/hadron-agent/profile
Wants=network-online.target
# The `agent` user/group and the profile are materialized in the cos-setup boot
# stage; order after it (and network) exactly as hadron-agent-provision does.
After=network-online.target cos-setup-boot.service cos-setup-network.service

[Service]
Type=oneshot
RemainAfterExit=yes
# ~2 GB download on a first boot over a slow link: give it room, but bounded so a
# hung mirror cannot wedge the unit forever (it retries next boot).
TimeoutStartSec=30min
ExecStart=/usr/bin/hadron-agent-browser-install

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 6: Commit**

```bash
chmod +x rootfs-agent/usr/bin/hadron-agent-browser-install
git add rootfs-agent/usr/bin/hadron-agent-browser-install \
        rootfs-agent/etc/systemd/system/hadron-agent-browser-install.service \
        test/agent/browser_install_test.sh
git commit -m "feat(agent): install Chromium system-wide on first boot

A root oneshot adds the image's signed Flathub remote and installs
org.chromium.Chromium (stable, unpinned) idempotently, gated on the agent
profile marker. Flatpak's own state is the source of truth, so it self-no-ops
once installed."
```

---

### Task 2: Persist the Flatpak install across reboots

Without this the ~2 GB install repeats every boot, since Kairos does not persist `/var/lib`.

**Files:**
- Create: `rootfs-agent/system/oem/94_agent_browser_persistence.yaml`
- Test: `test/agent/browser_persistence_test.sh`

**Interfaces:**
- Consumes: `/run/cos/cos-layout.env`'s `CUSTOM_BIND_MOUNTS` (already carrying `/var/lib/docker` from `92_docker.yaml` and `/var/lib/hadron-agent` from `93_agent_persistence.yaml`).
- Produces: `/var/lib/flatpak` appended to `CUSTOM_BIND_MOUNTS`. Task 1's install survives reboots.

- [ ] **Step 1: Write the failing test**

Create `test/agent/browser_persistence_test.sh`. It extracts the shell body from the yip stage and runs it against a fake `cos-layout.env` to prove it **appends** (never replaces) and is idempotent — the exact "don't clobber Docker's value" property `93_agent_persistence.yaml` was written to guarantee.

```bash
#!/usr/bin/env bash
# Tests for 94_agent_browser_persistence.yaml: appends /var/lib/flatpak to
# CUSTOM_BIND_MOUNTS without clobbering the entries 92_docker and 93_agent add,
# and is idempotent across re-runs of cos-setup.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
stage="$repo_root/rootfs-agent/system/oem/94_agent_browser_persistence.yaml"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# Pull the shell body out of the yip stage's `commands: - |` block. The block is
# indented 10 spaces (name/commands/-| nesting); strip that indent and run it.
extract_cmd() {
  awk '
    /commands:/ {inc=1; next}
    inc && /^[[:space:]]*- \|/ {body=1; next}
    body {
      if ($0 !~ /^[[:space:]]{10}/ && $0 !~ /^[[:space:]]*$/) {body=0; next}
      sub(/^[[:space:]]{10}/, ""); print
    }
  ' "$stage"
}

run_stage() { # $1 = layout file the stage should edit
  HOME=/dev/null layout="$1" sh -c "layout='$1'; $(extract_cmd)"
}

# Helper: what CUSTOM_BIND_MOUNTS ends up as.
value_of() { sed -n 's/^CUSTOM_BIND_MOUNTS="\(.*\)"$/\1/p' "$1"; }

# 1. appends alongside existing docker + hadron-agent entries
w="$(mktemp)"; printf 'CUSTOM_BIND_MOUNTS="/var/lib/docker /var/lib/hadron-agent"\n' > "$w"
run_stage "$w"
v="$(value_of "$w")"
case " $v " in
  *" /var/lib/docker "*) : ;; *) err "dropped /var/lib/docker (got: $v)";; esac
case " $v " in
  *" /var/lib/hadron-agent "*) : ;; *) err "dropped /var/lib/hadron-agent (got: $v)";; esac
case " $v " in
  *" /var/lib/flatpak "*) ok "appended /var/lib/flatpak, kept the rest";;
  *) err "did not append /var/lib/flatpak (got: $v)";; esac
rm -f "$w"

# 2. idempotent: a second run does not duplicate the entry
w="$(mktemp)"; printf 'CUSTOM_BIND_MOUNTS="/var/lib/docker"\n' > "$w"
run_stage "$w"; run_stage "$w"
count="$(value_of "$w" | tr ' ' '\n' | grep -cx '/var/lib/flatpak')"
if [ "$count" -eq 1 ]; then ok "idempotent (one /var/lib/flatpak entry)"; \
  else err "duplicated /var/lib/flatpak ($count times)"; fi
rm -f "$w"

# 3. empty/unset layout still yields a valid single entry
w="$(mktemp)"; : > "$w"
run_stage "$w"
if [ "$(value_of "$w")" = "/var/lib/flatpak" ]; then ok "empty layout -> single entry"; \
  else err "empty layout produced: $(value_of "$w")"; fi
rm -f "$w"

exit "$fail"
```

```bash
chmod +x test/agent/browser_persistence_test.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/agent/browser_persistence_test.sh
```

Expected: failures — the yip stage does not exist, so `extract_cmd` yields nothing and `CUSTOM_BIND_MOUNTS` is never updated.

- [ ] **Step 3: Write the persistence stage**

Create `rootfs-agent/system/oem/94_agent_browser_persistence.yaml`. It mirrors `93_agent_persistence.yaml`'s read-modify-write append verbatim, changing only the path. Sorted `94_` so it runs after `92_docker` and `93_agent` and sees their values. **No `if:` guard** — same reason `93_` documents (the `rootfs` stage runs in the initramfs context where `/etc` is not yet the installed one, and the file ships only in the agent overlay).

```yaml
# Persist Chromium's Flatpak install across reboots.
#
# Kairos does not persist /var/lib, so a system Flatpak install (/var/lib/flatpak)
# would be wiped every boot and hadron-agent-browser-install would re-download
# ~2 GB each time. This bind-mounts it onto the persistent partition, matching how
# 92_docker persists /var/lib/docker and 93_agent persists /var/lib/hadron-agent.
#
# The append is deliberate and mirrors 93_agent_persistence.yaml: cos-setup's
# rootfs.after stage consumes CUSTOM_BIND_MOUNTS once, so a plain `environment:`
# block would REPLACE the value and silently un-persist Docker and the agent
# state. This reads the current value, adds one path, and is a no-op if the path
# is already listed. 94_ sorts after 92_/93_ so their values are present first.
#
# No `if:` guard, for the same reason 93_ documents: the rootfs stage runs in the
# initramfs, where the installed /etc is not yet live, so a guard on
# /etc/hadron-agent/profile silently skips the stage. This file ships only in the
# agent overlay, so its presence is the profile check.
name: "Hadron agent Chromium persistence"
stages:
  rootfs:
    - name: "persist /var/lib/flatpak across reboots"
      commands:
        - |
          layout=/run/cos/cos-layout.env
          touch "$layout"
          # shellcheck source=/dev/null
          . "$layout" 2>/dev/null || true
          case " ${CUSTOM_BIND_MOUNTS:-} " in
            *" /var/lib/flatpak "*)
              exit 0
              ;;
          esac
          merged="${CUSTOM_BIND_MOUNTS:-} /var/lib/flatpak"
          sed -i '/^CUSTOM_BIND_MOUNTS=/d' "$layout"
          printf 'CUSTOM_BIND_MOUNTS="%s"\n' "${merged# }" >> "$layout"
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
test/agent/browser_persistence_test.sh
```

Expected: `ok: appended ...`, `ok: idempotent ...`, `ok: empty layout ...`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add rootfs-agent/system/oem/94_agent_browser_persistence.yaml \
        test/agent/browser_persistence_test.sh
git commit -m "feat(agent): persist /var/lib/flatpak across reboots

Mirror 93_agent_persistence's read-modify-write append so the Chromium install
survives reboots instead of re-downloading ~2 GB every boot. Sorted after the
docker and hadron-agent entries; no if: guard, same as 93."
```

---

### Task 3: Launch unit, wrapper, and session hook

Run Chromium as the `agent` user with the CDP port open, once the app is installed and the display is up, and restart it on crash.

**Files:**
- Create: `rootfs-agent/usr/bin/hadron-agent-browser-run`
- Create: `rootfs-agent/usr/lib/systemd/user/hadron-agent-browser.service`
- Modify: `rootfs-agent/usr/bin/hadron-agent-session-ready`
- Test: `test/agent/browser_run_test.sh`

**Interfaces:**
- Consumes: `flatpak info --system org.chromium.Chromium` (Task 1); DISPLAY/XAUTHORITY imported into the `agent` user manager by `hadron-agent-session-ready`.
- Produces: a Chromium process listening on `127.0.0.1:9222`, which `agent/internal/cua/browser.go` (`browserCDPPort = 9222`) connects to.

- [ ] **Step 1: Write the failing test**

Create `test/agent/browser_run_test.sh`. It drives the wrapper with a fake flatpak and asserts: it refuses to launch when the app is not installed (so it does not thrash before Task 1 finishes on first boot), refuses when DISPLAY is unset, and otherwise `exec`s `flatpak run` with the CDP port and the required hardening flags.

```bash
#!/usr/bin/env bash
# Tests for hadron-agent-browser-run: the launch wrapper the user unit execs.
# A fake flatpak records the `run` argv and answers `info` from a control file,
# so we assert the launch flags without starting a real browser.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/rootfs-agent/usr/bin/hadron-agent-browser-run"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

if sh -n "$script" 2>/dev/null; then ok "sh -n"; else err "sh -n failed"; fi

make_fake_flatpak() { # $1 = dir; presence of $1/installed => info exits 0
  d="$1"
  cat > "$d/flatpak" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$d/calls"
case "\$1 \$2" in
  "info --system") [ -e "$d/installed" ] && exit 0 || exit 1 ;;
  "run "*) : > "$d/ran" ;;    # would exec; record and return so the test continues
esac
exit 0
EOF
  chmod +x "$d/flatpak"
}

# 1. app not installed -> does not run, exits non-zero (unit retries)
d="$(mktemp -d)"; make_fake_flatpak "$d"
if DISPLAY=:0 HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
     sh "$script" >/dev/null 2>&1; then
  err "must not launch before the app is installed"
else ok "app absent -> non-zero, no launch"; fi
[ -e "$d/ran" ] && err "launched despite app absent"
rm -rf "$d"

# 2. no DISPLAY -> does not run, exits non-zero
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
if env -u DISPLAY HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
     sh "$script" >/dev/null 2>&1; then
  err "must not launch with no DISPLAY"
else ok "no DISPLAY -> non-zero, no launch"; fi
rm -rf "$d"

# 3. installed + DISPLAY -> execs flatpak run with the CDP port and hardening flags
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
DISPLAY=:0 HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
  sh "$script" >/dev/null 2>&1
calls="$(cat "$d/calls" 2>/dev/null)"
for needle in "run" "org.chromium.Chromium" "--remote-debugging-port=9222" \
              "--no-sandbox" "--ozone-platform=x11"; do
  case "$calls" in *"$needle"*) : ;; *) err "launch missing $needle (got: $calls)";; esac
done
[ -e "$d/ran" ] && ok "installed + DISPLAY -> flatpak run with CDP port"
rm -rf "$d"

exit "$fail"
```

```bash
chmod +x test/agent/browser_run_test.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/agent/browser_run_test.sh
```

Expected: `FAIL: sh -n failed` plus the behavioural cases failing — the wrapper does not exist.

- [ ] **Step 3: Write the launch wrapper**

Create `rootfs-agent/usr/bin/hadron-agent-browser-run`. The `HADRON_BROWSER_EXEC` seam lets tests bypass the real `exec` (a fake flatpak returns instead of replacing the process).

```sh
#!/bin/sh
# Launch Chromium for the Cua agent with the CDP debug port open.
#
# Run as the `agent` user by hadron-agent-browser.service (a systemd --user unit).
# It guards on two preconditions and exits non-zero if either is unmet, so the
# unit's Restart=always retries rather than the browser crash-looping:
#   1. the app is installed (hadron-agent-browser-install may still be running on
#      a first boot) -- checked via flatpak's own state, no network;
#   2. DISPLAY is set (hadron-agent-session-ready imports it into the user manager
#      once the desktop is up).
#
# Flags are taken verbatim from the proven test fixture
# (test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf and test/agent/run.sh):
# software GL and no-sandbox suit the appliance VM; --remote-debugging-port=9222
# is where agent/internal/cua/browser.go connects; --force-renderer-accessibility
# exposes the a11y tree the tool relies on.
#
# Seams (tests only):
#   HADRON_FLATPAK       flatpak binary (default "flatpak").
#   HADRON_BROWSER_EXEC  if set, do not exec -- return after invoking flatpak, so
#                        tests can assert the argv without replacing the process.
set -eu

FLATPAK="${HADRON_FLATPAK:-flatpak}"
APP=org.chromium.Chromium

if ! "$FLATPAK" info --system "$APP" >/dev/null 2>&1; then
    echo "$APP not installed yet; will retry" >&2
    exit 1
fi
if [ -z "${DISPLAY:-}" ]; then
    echo "DISPLAY not set yet; will retry" >&2
    exit 1
fi

set -- "$FLATPAK" run \
    --socket=x11 --socket=session-bus \
    --env=DISPLAY="$DISPLAY" \
    ${XAUTHORITY:+--env=XAUTHORITY="$XAUTHORITY" --filesystem="$XAUTHORITY"} \
    "$APP" \
    --ozone-platform=x11 --no-sandbox --disable-gpu --disable-dev-shm-usage \
    --no-first-run --force-renderer-accessibility \
    --remote-debugging-port=9222 --remote-allow-origins=* \
    --new-window about:blank

if [ -n "${HADRON_BROWSER_EXEC:-}" ]; then
    "$@"        # test path: fake flatpak records argv and returns
else
    exec "$@"   # production: replace this process with Chromium
fi
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
test/agent/browser_run_test.sh
```

Expected: `ok: sh -n`, `ok: app absent ...`, `ok: no DISPLAY ...`, `ok: installed + DISPLAY ...`, exit 0.

- [ ] **Step 5: Write the user launch unit**

Create `rootfs-agent/usr/lib/systemd/user/hadron-agent-browser.service`. It mirrors `hadron-agent-session.service`'s `ConditionUser=agent` gating, follows the session for restart (`PartOf`/`After`), and is **not** `WantedBy` anything — `hadron-agent-session-ready` starts it explicitly once the display env is imported (Step 6), so it never starts before DISPLAY exists.

```ini
# Chromium for the Cua agent, in the `agent` user's manager. Gated ConditionUser
# =agent so it is a no-op in every other user's --user instance.
#
# It is started explicitly by hadron-agent-session-ready AFTER DISPLAY/XAUTHORITY
# are imported into this manager, so the wrapper always has a display; and it is
# PartOf the session broker, so when the display watchdog restarts the session on
# a display change, Chromium restarts with it. Restart=always covers crashes and
# the first-boot window where the app is still installing (the wrapper exits
# non-zero until both the app and DISPLAY are present).
[Unit]
Description=Hadron agent Chromium (CDP on 127.0.0.1:9222)
ConditionUser=agent
After=hadron-agent-session.service
PartOf=hadron-agent-session.service

[Service]
Type=simple
ExecStart=/usr/bin/hadron-agent-browser-run
Restart=always
RestartSec=3s
```

- [ ] **Step 6: Hook the launch into session bring-up**

`hadron-agent-session-ready` already imports the display env into the `agent` user manager and restarts the session broker. Add one guarded line so the browser starts on the live display. Read the file first:

```bash
sed -n '30,40p' rootfs-agent/usr/bin/hadron-agent-session-ready
```

After the existing `systemctl --user restart hadron-agent-session.service` line, add:

```sh
# Bring up the agent's browser on the now-live display. Guarded so a failure here
# never blocks the session broker (the browser unit's Restart=always will retry).
systemctl --user start hadron-agent-browser.service || true
```

- [ ] **Step 7: Verify the session-ready edit did not break the script**

```bash
sh -n rootfs-agent/usr/bin/hadron-agent-browser-run
sh -n rootfs-agent/usr/bin/hadron-agent-session-ready
```

Expected: both silent (exit 0). A syntax error in `session-ready` would break the whole agent session, so this check is load-bearing.

- [ ] **Step 8: Commit**

```bash
chmod +x rootfs-agent/usr/bin/hadron-agent-browser-run
git add rootfs-agent/usr/bin/hadron-agent-browser-run \
        rootfs-agent/usr/lib/systemd/user/hadron-agent-browser.service \
        rootfs-agent/usr/bin/hadron-agent-session-ready \
        test/agent/browser_run_test.sh
git commit -m "feat(agent): launch Chromium with the CDP debug port

A per-user (agent) unit runs Chromium with --remote-debugging-port=9222 once the
app is installed and the display is up, restarting on crash. session-ready starts
it on the live display; PartOf the session so it follows display restarts."
```

---

### Task 4: Image wiring and end-to-end verification

Enable the install unit in the agent image, ship the user unit into the global user-manager, and verify the whole chain on a booted appliance.

**Files:**
- Modify: `Dockerfile.agent`
- Test: `test/agent/browser_units_test.sh`

**Interfaces:**
- Consumes: everything from Tasks 1-3.
- Produces: the enabled units in the built `agent-desktop:dev` image. Terminal task.

- [ ] **Step 1: Find how the agent overlay is applied and units enabled**

The agent overlay is copied and its units enabled in `Dockerfile.agent`. Locate the existing enablement of an agent system unit and the user-unit install, to mirror them:

```bash
grep -nE "rootfs-agent|systemctl enable|multi-user.target.wants|systemd/user|hadron-agent-(provision|gateway)" Dockerfile.agent
```

Note the pattern used for an existing system oneshot (e.g. `hadron-agent-provision.service`) — a committed symlink into `multi-user.target.wants` or a `systemctl enable`. User units under `usr/lib/systemd/user/` are enabled globally with `systemctl --global enable`.

- [ ] **Step 2: Write the failing test**

Create `test/agent/browser_units_test.sh`, an image check in the style of `test/boot-ux/check-*.sh` (docker-run assertions against the built agent image).

```bash
#!/usr/bin/env bash
# Assert the Chromium install/launch units are present and enabled in the built
# agent image, and that the signed Flathub descriptor they depend on is shipped.
set -euo pipefail
IMAGE="${1:-agent-desktop:dev}"
fail=0
check() {
  local desc="$1"; shift
  if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
    echo "PASS: $desc"
  else echo "FAIL: $desc"; fail=1; fi
}

check "install script present and executable" 'test -x /usr/bin/hadron-agent-browser-install'
check "launch wrapper present and executable" 'test -x /usr/bin/hadron-agent-browser-run'
check "install unit present"        'test -f /etc/systemd/system/hadron-agent-browser-install.service'
check "install unit enabled (multi-user.target.wants)" \
  'test -L /etc/systemd/system/multi-user.target.wants/hadron-agent-browser-install.service'
check "launch user unit present"    'test -f /usr/lib/systemd/user/hadron-agent-browser.service'
check "signed Flathub descriptor shipped" 'test -f /usr/share/hadron/flathub.flatpakrepo'
check "flatpak available in the image" 'command -v flatpak'
check "session-ready starts the browser unit" \
  'grep -q "systemctl --user start hadron-agent-browser.service" /usr/bin/hadron-agent-session-ready'

exit "$fail"
```

```bash
chmod +x test/agent/browser_units_test.sh
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
make agent-image 2>/dev/null || true
test/agent/browser_units_test.sh agent-desktop:dev
```

Expected: `FAIL` on the script/unit checks (not yet wired), likely `PASS` on the descriptor and flatpak checks (inherited from the base image).

- [ ] **Step 4: Confirm enablement and that the user unit ships**

The install unit is **already enabled** by the committed symlink
`rootfs-agent/etc/systemd/system/multi-user.target.wants/hadron-agent-browser-install.service`
added in Task 1 — that is how every agent system unit is enabled here (the
overlay is a plain `COPY rootfs-agent/ /` in `Dockerfile.agent`, which never
processes a unit's `[Install]` section, so a committed `.wants` symlink is the
only enablement mechanism). **Do not add a `systemctl enable` or a Dockerfile
`RUN` symlink** — that would be a redundant second mechanism. Just verify the
symlink survives into the built image:

```bash
docker run --rm agent-desktop:dev test -L \
  /etc/systemd/system/multi-user.target.wants/hadron-agent-browser-install.service && echo OK
```

The user unit `usr/lib/systemd/user/hadron-agent-browser.service` needs no global
enable — it is started explicitly by `hadron-agent-session-ready` (Task 3 Step 6)
and gated `ConditionUser=agent`; it is intentionally not `WantedBy` anything.
Verify the overlay copy places it at `/usr/lib/systemd/user/` in the image (the
`browser_units_test.sh` check covers this). No `Dockerfile.agent` change should
be needed for enablement; the only edits this task makes to `Dockerfile.agent`
are none unless the descriptor/flatpak checks in Step 2 reveal a genuine gap.

- [ ] **Step 5: Rebuild and run the test to verify it passes**

```bash
make agent-image
test/agent/browser_units_test.sh agent-desktop:dev
```

Expected: eight `PASS` lines, exit 0.

- [ ] **Step 6: Wire the shell tests into the agent test runner**

The three shell unit tests from Tasks 1-3 must run in the existing agent harness. Find where `test/agent/*_test.sh` are invoked:

```bash
grep -rnE "_test\.sh|for .*test|run_unit" test/agent/run.sh test/agent/*.sh | grep -i test | head
```

Add `browser_install_test.sh`, `browser_persistence_test.sh`, and `browser_run_test.sh` to whatever list/loop runs the other `*_test.sh` unit tests, matching the existing invocation style. If they are auto-discovered by glob, confirm the new files are picked up:

```bash
for t in test/agent/browser_*_test.sh; do echo "== $t"; "$t" || echo "FAILED"; done
```

Expected: each prints its `ok:` lines and exits 0.

- [ ] **Step 7: End-to-end boot verification**

This is the payoff and the one check nothing else covers: on a booted appliance the units bring Chromium up on 9222 by themselves. Build the agent ISO and boot it (the repo's `tools/vm.sh` / the `test/agent/run.sh` harness do headless QEMU with QMP; earlier work in this repo drove installs via QMP screendumps, so this is feasible).

```bash
make agent-iso
# install + boot the appliance (see test/agent/run.sh for the QMP harness)
```

On the booted, installed appliance, confirm — do not fabricate; report exactly what you observe:
1. `systemctl status hadron-agent-browser-install.service` reached `active (exited)` (or is still downloading on a fresh first boot — note which).
2. `flatpak info --system org.chromium.Chromium` succeeds.
3. After the desktop is up, `curl -s http://127.0.0.1:9222/json/version` returns Chromium's CDP banner — proving the launch unit ran on the live display and the port is open.
4. Reboot; confirm Chromium is still installed (no re-download) — i.e. `/var/lib/flatpak` persisted (Task 2).

If the boot cannot complete in the environment, report exactly what blocked it rather than claiming a pass. Items 3 and 4 are the ones that prove the feature; a green shell/image suite does not.

- [ ] **Step 8: Commit**

```bash
git add Dockerfile.agent test/agent/browser_units_test.sh test/agent/run.sh
git commit -m "feat(agent): wire first-boot Chromium into the agent image

Enable the install oneshot in Dockerfile.agent and run the browser unit tests in
the agent harness. The Cua browser tool now has a browser on 127.0.0.1:9222 on a
fresh appliance with no human intervention."
```

---

## Verification checklist

After all four tasks:

```bash
for t in test/agent/browser_*_test.sh; do "$t"; done
make agent-image && test/agent/browser_units_test.sh agent-desktop:dev
make agent-iso   # then boot per test/agent/run.sh and check 9222
```

The booted check is the real test: `curl 127.0.0.1:9222/json/version` returns a CDP banner on a fresh appliance, and Chromium survives a reboot without re-downloading.

## Risks

| Risk | Mitigation |
|---|---|
| Flathub unreachable on first boot | Install unit fails and retries next boot; boot never blocked; appliance usable, browser tool just errors until then |
| `94_` clobbers the docker/hadron-agent bind mounts | Read-modify-write append, idempotent, sorted after `92_`/`93_` — asserted by `browser_persistence_test.sh` |
| Launch thrashes before the first-boot install finishes | Wrapper exits non-zero (no launch) until `flatpak info` succeeds; `Restart=always` retries calmly on `RestartSec=3s` |
| `stable` drift across a fleet (D3) | Accepted trade for first-boot resilience; test harness stays pinned; flip to a build-arg pin if reproducibility is required |
| A syntax error in `session-ready` breaks the whole session | `sh -n` gate (Task 3 Step 7); the added line is `|| true`-guarded so a runtime failure can't block the broker |
| `/var/lib/flatpak` persistence entry silently skipped | Mirrors `93_` exactly, including the no-`if:`-guard rationale; `browser_persistence_test.sh` proves the append |
| 2 GB install on a small disk | Documented disk requirement (≥32 GB, per the test README); install failure is non-fatal |

## References

- `test/agent/Dockerfile.compat` — the proven `flatpak --system remote-add` + install recipe
- `test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf`, `test/agent/run.sh` — the exact launch flags and CDP port
- `rootfs-agent/system/oem/93_agent_persistence.yaml` — the `CUSTOM_BIND_MOUNTS` append pattern this mirrors
- `rootfs-agent/etc/systemd/system/hadron-agent-provision.service` — first-boot oneshot conventions
- `rootfs-agent/usr/lib/systemd/user/hadron-agent-session.service`, `rootfs-agent/usr/bin/hadron-agent-session-ready` — the `agent` user manager and the display-env import hook
- `rootfs/usr/bin/hadron-user-setup` — the signed Flathub remote (descriptor path, fingerprint, URL)
- `agent/internal/cua/browser.go` — `browserCDPPort = 9222`, connect-only
