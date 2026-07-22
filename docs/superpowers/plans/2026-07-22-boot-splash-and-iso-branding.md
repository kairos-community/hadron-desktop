# Boot Splash and ISO Branding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make hadron-desktop paint a customized splash continuously from early initramfs through to the ly login, behind a branded live ISO boot menu.

**Architecture:** A vendored C99 splash binary (`splash/main.c`, forked from the upstream splash by way of AIOS) is built in a toolchain stage and installed to `/usr/bin/hadron-splash`, overwriting the base image's stock copy. A new dracut module bakes that same binary into the initramfs so the animation starts before switch-root; the existing rootfs unit continues it before ly. A new `auroraboot/build.sh` stages an `--overlay-iso` directory carrying our own live `grub.cfg` plus the GRUB theme, which is what finally gives us a place to set the quiet kernel cmdline the splash needs.

**Tech Stack:** C99 (musl, `ghcr.io/kairos-io/hadron-toolchain:main`), dracut, systemd, GRUB 2 gfxterm, AuroraBoot `v0.21.0-alpha.4`, Docker BuildKit, POSIX sh / bash.

**Spec:** `docs/superpowers/specs/2026-07-22-boot-and-install-ux-design.md` (Parts 1 and 2. Part 3, the installer TUI, is a separate plan.)

## Global Constraints

- **VGA-16 colours only** in anything rendered on the kernel VT. The console is a 16-colour VGA framebuffer; 256-colour and truecolor values round to the wrong hue. Allowed: `4` blue, `8` dark grey, `12` bright blue, `14` bright cyan, `15` white. Never emit 256-colour SGR sequences from the splash.
- **All new binaries go in `/usr/bin`**, never `/usr/local/bin` — `/usr/local` is shadowed by an empty overlay at runtime on Kairos.
- **Best-effort GRUB assets.** Every `loadfont` / `set theme` must be guarded by an `if [ -f ... ]` so a missing asset degrades to the plain text menu and can never block boot. This matches the discipline already in `rootfs/etc/kairos/branding/grubmenu.cfg`.
- **kairos-init clobbers branding files.** Anything under `/etc/issue`, `/etc/motd`, `/etc/vconsole.conf`, `/etc/kairos/branding/grubmenu.cfg` must be re-`COPY`d *after* the kairos-init stage.
- **The splash binary must be COPY'd into the image before kairos-init runs `dracut -f`**, or the dracut module has nothing to install.
- **AuroraBoot pinned at `quay.io/kairos/auroraboot:v0.21.0-alpha.4`** (`AURORA_IMAGE` in the Makefile). Do not bump it in this plan.
- Existing `AURORA_IMAGE`, `BASE_IMAGE`, `DESKTOP`, `VERSION`, `GPU`, `FIRMWARE` Makefile knobs keep working unchanged.
- Full quiet cmdline, used verbatim everywhere it appears:
  `quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false`

---

### Task 1: Vendored splash source and build stage

Fork the upstream splash into the repo, rebrand it for hadron-desktop, and build it in a toolchain stage. Nothing is wired into boot yet — this task ends with a binary in the image that replaces the base image's stock one.

**Files:**
- Create: `splash/main.c`
- Create: `splash/Makefile`
- Create: `splash/README.md`
- Modify: `Dockerfile` (new `hadron-splash` stage; `COPY` into the desktop stage)
- Test: `test/boot-ux/check-splash.sh`

**Interfaces:**
- Consumes: nothing.
- Produces: `/usr/bin/hadron-splash` in the built image — a dynamically-linked musl ELF that animates for ~5s on a tty and prints `HADRON` (optionally followed by the version from `/etc/os-release`) when stdout is not a tty or the terminal is smaller than 58x12. Task 2 bakes this exact path into the initramfs.

- [ ] **Step 1: Vendor the upstream source**

Copy AIOS's splash as the starting point — it is already a rebrand of the same upstream program, so this is a retheme rather than a rewrite:

```bash
mkdir -p splash
cp /home/mudler/_git/AIOS/image/aios-splash/main.c splash/main.c
cp /home/mudler/_git/AIOS/image/aios-splash/Makefile splash/Makefile
```

- [ ] **Step 2: Rename the program in the Makefile**

Replace the whole of `splash/Makefile` with:

```make
.PHONY: all clean
all: hadron-splash

hadron-splash: main.c
	@gcc -Os -std=c99 -Wall -Wextra -Wpedantic \
		-fno-asynchronous-unwind-tables -fno-unwind-tables -fno-stack-protector \
		-ffunction-sections -fdata-sections -fno-plt -fno-pie -no-pie -flto \
		-Wl,--gc-sections -Wl,--build-id=none -Wl,-z,noseparate-code -Wl,-z,norelro \
		-Wl,--hash-style=gnu -s -o $@ $< -lm

clean:
	@rm -f hadron-splash *.o
```

- [ ] **Step 3: Retheme the palette**

In `splash/main.c`, replace the `AMBER` array and its comment (currently around line 28) with the Tokyo Night ramp. Note the array is indexed by the `A_*` defines below it, so the ordering matters: index 1 is faint chrome, 2 is the dim tagline/version, 4 is the logo body, 6 is the scanline shoulder, 7 is the crest.

```c
/* Tokyo Night ramp, dim -> hot. The kernel VT/framebuffer is a 16-color VGA
   console: the truecolor Tokyo Night values (#7aa2f7, #7dcfff, #565f89) have no
   256-color equivalent that survives the round-trip, so we use the colors the
   VGA palette actually has — 4 = blue, 8 = dark grey (the #565f89 comment hue),
   12 = bright blue (#7aa2f7), 14 = bright cyan (#7dcfff), 15 = white-hot. */
static const uint8_t TOKYO[8] = {4, 4, 8, 12, 12, 12, 14, 15};
```

Then rename every remaining use of the old array:

```bash
sed -i 's/\bAMBER\b/TOKYO/g' splash/main.c
```

- [ ] **Step 4: Replace the wordmark**

Replace the `ART_ROWS` / `ART_COLS` / `ART` block (currently around line 93) with the ANSI-Shadow "HADRON". `ART_COLS` is **display columns, not bytes** — `paint_logo` decodes UTF-8 via `utf8_next` and increments its column counter once per glyph, so 51 is correct even though each row is far longer in bytes.

```c
#define ART_ROWS 6
#define ART_COLS 51
static const char *ART[ART_ROWS] = {
"██╗  ██╗ █████╗ ██████╗ ██████╗  ██████╗ ███╗   ██╗",
"██║  ██║██╔══██╗██╔══██╗██╔══██╗██╔═══██╗████╗  ██║",
"███████║███████║██║  ██║██████╔╝██║   ██║██╔██╗ ██║",
"██╔══██║██╔══██║██║  ██║██╔══██╗██║   ██║██║╚██╗██║",
"██║  ██║██║  ██║██████╔╝██║  ██║╚██████╔╝██║ ╚████║",
"╚═╝  ╚═╝╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝",
};
```

- [ ] **Step 5: Raise the minimum terminal width**

`MIN_COLS` must exceed the art width plus the HUD bracket margin — `render` computes `fr_l = left - 3` and `fr_r = left + ART_COLS + 2`, so the art needs `ART_COLS + 6` columns to draw its frame. Change the define (currently around line 25) from `34` to:

```c
#define MIN_COLS     58
```

Leave `MIN_ROWS 12` alone.

- [ ] **Step 6: Replace the tagline and boot lines**

Replace `TAG` and `BOOT` (currently around lines 103-111). The tagline is deliberately variant-neutral — a single common splash serves both sway and i3, and hardcoding one variant is exactly the defect Task 3 fixes in the GRUB theme.

```c
static const char *TAG = "the immutable tiling desktop";
/* In-progress, honest boot phrases — true during early boot, never claiming a
   service is "ready" before it is. */
static const char *BOOT[3] = {
    "waking the machine",
    "mounting the immutable core",
    "bringing up the desktop",
};
```

- [ ] **Step 7: Fix the non-tty fallback string**

The fallback path (currently around line 193) still prints `AIOS`. Replace that line with:

```c
        if (g_ver[0]) printf("HADRON %s\n", g_ver); else puts("HADRON");
```

Then confirm no stale branding survives anywhere in the file:

```bash
grep -in "aios" splash/main.c
```

Expected: no output. If anything matches (including the file's header comment), rewrite it to describe hadron-desktop.

- [ ] **Step 8: Write the failing test**

Create `test/boot-ux/check-splash.sh`. This asserts the two properties that can be checked without booting: the binary exists and is executable, and its non-tty fallback prints the brand. It runs against a built image.

```bash
#!/usr/bin/env bash
# Assert the customized splash is present and sane in a built desktop image.
#
#   test/boot-ux/check-splash.sh [IMAGE]
#
# Runs the splash with stdout redirected to a pipe (not a tty), which takes the
# fallback path in main() — that is both a brand check and a crash check, since
# the whole program is linked and the version reader runs before the fallback.
set -euo pipefail
IMAGE="${1:-sway-desktop:dev}"
fail=0
check() {
    local desc="$1"; shift
    if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"; fail=1
    fi
}

check "hadron-splash present and executable" 'test -x /usr/bin/hadron-splash'
check "hadron-splash non-tty fallback prints HADRON" '/usr/bin/hadron-splash | grep -q HADRON'
check "no stale AIOS branding in the binary" '! strings /usr/bin/hadron-splash | grep -qi aios'

exit "$fail"
```

Make it executable:

```bash
chmod +x test/boot-ux/check-splash.sh
```

- [ ] **Step 9: Run the test to verify it fails**

```bash
test/boot-ux/check-splash.sh sway-desktop:dev
```

Expected: `FAIL: no stale AIOS branding in the binary` is unlikely, but `FAIL: hadron-splash non-tty fallback prints HADRON` **must** appear if you have an image built before this task — the stock base-image splash prints its own upstream branding. If no image exists yet, the run fails on `docker run` with "Unable to find image"; build one first with `make image` and re-run to see a genuine assertion failure.

- [ ] **Step 10: Add the build stage to the Dockerfile**

Insert a new stage immediately after the `toolchain` stage declaration (currently line 26, `FROM ghcr.io/kairos-io/hadron-toolchain:main AS toolchain`). Placing it here keeps it independent of the graphics-stack stages so BuildKit can schedule it in parallel:

```dockerfile
# ---------------------------------------------------------------------------
# Boot splash. A vendored C99 fork of the upstream hadron splash, rebranded for
# hadron-desktop (Tokyo Night VGA-16 ramp, ANSI-Shadow HADRON wordmark). The
# same binary serves BOTH the initramfs copy (baked in by the 50hadron-splash
# dracut module) and the booted system's hadron-splash.service, so there is one
# source of truth for the animation.
#
# The build-time smoke test exercises the non-tty fallback path in main(), which
# links and runs the whole program — a segfault or a missing symbol fails the
# build here rather than at boot, where it would be invisible behind `quiet`.
# ---------------------------------------------------------------------------
FROM toolchain AS hadron-splash
COPY splash/ /build/splash/
RUN cd /build/splash && make clean && make && ./hadron-splash | grep -q HADRON
```

- [ ] **Step 11: Install the binary into the desktop stage**

The binary must be in the image *before* the kairos-init stage runs `dracut -f`, and it must overwrite the base image's stock `/usr/bin/hadron-splash`. Add this `COPY` immediately before the `RUN` block that enables `hadron-splash.service` (the block containing `systemctl enable hadron-splash.service`, currently around line 2718):

```dockerfile
# Overwrite the base image's stock splash with our rebranded build. Must land
# before the kairos-init stage runs `dracut -f`, so the 50hadron-splash dracut
# module (Task 2) bakes THIS binary into the initramfs rather than the stock one.
COPY --from=hadron-splash /build/splash/hadron-splash /usr/bin/hadron-splash
```

- [ ] **Step 12: Rebuild and run the test to verify it passes**

```bash
make image
test/boot-ux/check-splash.sh sway-desktop:dev
```

Expected: three `PASS` lines, exit 0.

- [ ] **Step 13: Document the fork**

Create `splash/README.md`:

```markdown
# hadron-desktop boot splash

A C99 fork of the upstream Kairos hadron splash (by way of
[AIOS](https://github.com/mudler/AIOS)'s `image/aios-splash`), rebranded for
hadron-desktop.

Built by the `hadron-splash` stage in the top-level `Dockerfile` and installed
to `/usr/bin/hadron-splash`, overwriting the base image's stock copy. The same
binary is used twice:

- **initramfs** — baked in by the `50hadron-splash` dracut module
  (`rootfs/usr/lib/dracut/modules.d/50hadron-splash/`), so the animation starts
  before switch-root.
- **booted system** — `rootfs/usr/lib/systemd/system/hadron-splash.service`,
  ordered `Before=ly@tty1.service`.

## Constraints

- **VGA-16 colours only.** The kernel VT is a 16-colour console; truecolor and
  256-colour values round to the wrong hue. See the `TOKYO` ramp in `main.c`.
- **`ART_COLS` is display columns, not bytes.** `paint_logo` decodes UTF-8 and
  counts one column per glyph.
- **`MIN_COLS` must be at least `ART_COLS + 6`** — the HUD brackets are drawn at
  `left - 3` and `left + ART_COLS + 2`.
- The box-drawing and block glyphs need a console font that covers them; see
  `rootfs/etc/vconsole.conf`.

## Build locally

    cd splash && make && ./hadron-splash
```

- [ ] **Step 14: Commit**

```bash
git add splash/ test/boot-ux/check-splash.sh Dockerfile
git commit -m "feat(splash): vendor and rebrand the boot splash

Fork the upstream splash into splash/ with a Tokyo Night VGA-16 ramp and an
ANSI-Shadow HADRON wordmark, build it in a toolchain stage, and overwrite the
base image's stock /usr/bin/hadron-splash."
```

---

### Task 2: Initramfs coverage via a dracut module

Bake the splash into the initramfs so it paints from early boot, and take immucore's logs off the console so they don't print over it. This is the core of the feature.

**Files:**
- Create: `rootfs/etc/dracut.conf.d/50-hadron-splash.conf`
- Create: `rootfs/usr/lib/dracut/modules.d/50hadron-splash/module-setup.sh`
- Create: `rootfs/usr/lib/dracut/modules.d/50hadron-splash/hadron-splash.service`
- Create: `rootfs/usr/lib/dracut/modules.d/50hadron-splash/immucore-quiet.conf`
- Modify: `Makefile` (cache-bust the splash and kairos-init stages)
- Test: `test/boot-ux/check-initramfs.sh`

**Interfaces:**
- Consumes: `/usr/bin/hadron-splash` from Task 1.
- Produces: `/boot/initrd` in the built image containing `usr/bin/hadron-splash`, an `initrd.target.requires/hadron-splash.service` symlink, and `immucore.service.d/10-quiet.conf`. No later task depends on these paths.

- [ ] **Step 1: Write the failing test**

Create `test/boot-ux/check-initramfs.sh`. The load-bearing assertion is the last one: the overlay files being present in the image proves nothing, because the whole feature fails silently if dracut didn't actually pick the module up.

```bash
#!/usr/bin/env bash
# Assert the splash is baked into the image's initramfs.
#
#   test/boot-ux/check-initramfs.sh [IMAGE]
#
# The overlay-file checks are cheap sanity; the initramfs content check is the
# one that matters. Two known failure modes make the module silently no-op:
#   1. the splash binary landing in the image AFTER kairos-init runs `dracut -f`
#   2. buildx caching the kairos-init stage, shipping a stale initramfs
# Both leave the overlay files present and the initramfs without the splash.
set -euo pipefail
IMAGE="${1:-sway-desktop:dev}"
fail=0
check() {
    local desc="$1"; shift
    if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"; fail=1
    fi
}

M=/usr/lib/dracut/modules.d/50hadron-splash
check "dracut conf.d drop-in present"   "test -f /etc/dracut.conf.d/50-hadron-splash.conf"
check "module-setup.sh present and executable" "test -x $M/module-setup.sh"
check "initramfs unit present"          "test -f $M/hadron-splash.service"
check "immucore quiet drop-in present"  "test -f $M/immucore-quiet.conf"
check "initrd exists"                   "test -f /boot/initrd"
check "splash baked into the initramfs" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'usr/bin/hadron-splash'"
check "initramfs unit activated via initrd.target.requires" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'initrd.target.requires/hadron-splash.service'"

exit "$fail"
```

```bash
chmod +x test/boot-ux/check-initramfs.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/boot-ux/check-initramfs.sh sway-desktop:dev
```

Expected: `FAIL` on all four overlay-file checks and both `lsinitrd` checks; `PASS` only on "initrd exists".

If `lsinitrd` is not present in the image, substitute the extraction form in the script (`/usr/lib/dracut/skipcpio /boot/initrd | zstd -d | cpio -t`) and re-run — but check first, since kairos-init images normally ship `lsinitrd`.

- [ ] **Step 3: Write the dracut conf.d drop-in**

Create `rootfs/etc/dracut.conf.d/50-hadron-splash.conf`:

```
# Force-include the hadron-splash dracut module so the animated splash is baked
# into /boot/initrd and paints from early boot. The module lives at
# /usr/lib/dracut/modules.d/50hadron-splash. kairos-init runs `dracut -f /boot/initrd`
# during the image build (after our overlay + the hadron-splash binary are in
# place), which reads this drop-in and pulls the module in.
add_dracutmodules+=" hadron-splash "
```

- [ ] **Step 4: Write the dracut module setup script**

Create `rootfs/usr/lib/dracut/modules.d/50hadron-splash/module-setup.sh`:

```bash
#!/bin/bash
# hadron-desktop early-boot splash — dracut module.
#
# Paints the animated splash from inside the initramfs (on tty1), so the
# animation starts at early boot instead of only just before the login prompt.
# Structure mirrors kairos-init's own 28immucore module (the canonical pattern
# for adding a binary + a systemd unit to the Kairos initramfs).
#
# The unit dies at switch-root (Conflicts=initrd-switch-root.target); the booted
# system's hadron-splash.service (ordered Before=ly@tty1.service) then continues
# the splash. `quiet` on the kernel cmdline keeps the hand-off gap clean.
#
# hadron-splash is a dynamic musl binary, so inst_multiple is used (like
# immucore) to pull the binary AND its shared-library deps into the initramfs.
# inst_simple would copy the ELF without its libs and it would fail to exec.

# check() decides whether dracut pulls this module in. 0 = include in the default
# set; the /etc/dracut.conf.d drop-in also force-adds it via add_dracutmodules.
check() {
    require_binaries /usr/bin/hadron-splash || return 1
    return 0
}

# No dependencies on other dracut modules.
depends() {
    return 0
}

install() {
    inst_multiple /usr/bin/hadron-splash
    inst_simple "${moddir}/hadron-splash.service" "${systemdsystemunitdir}/hadron-splash.service"
    # Activate the unit inside the initramfs by wiring it into initrd.target, the
    # same way 28immucore activates immucore.service.
    mkdir -p "${initdir}/${systemdsystemunitdir}/initrd.target.requires"
    ln_r "../hadron-splash.service" "${systemdsystemunitdir}/initrd.target.requires/hadron-splash.service"

    # Drop-in that takes immucore's logs off the console so they don't print over
    # the splash. 28immucore installs immucore.service into the same initdir;
    # this only adds a .d override, so module ordering does not matter.
    mkdir -p "${initdir}/${systemdsystemunitdir}/immucore.service.d"
    inst_simple "${moddir}/immucore-quiet.conf" "${systemdsystemunitdir}/immucore.service.d/10-quiet.conf"
}
```

- [ ] **Step 5: Write the initramfs unit**

Create `rootfs/usr/lib/dracut/modules.d/50hadron-splash/hadron-splash.service`. This is the **initramfs** copy and is deliberately different from the rootfs unit at `rootfs/usr/lib/systemd/system/hadron-splash.service` — do not confuse the two. This one is `Type=simple` (it runs alongside immucore's mounting rather than blocking it) and has no `[Install]` section, because the module wires it into `initrd.target.requires` directly.

```ini
[Unit]
Description=Hadron desktop early boot splash (initramfs)
# Run inside the initramfs with no implicit ordering deps, as early as the console
# is usable. After udev has triggered, /dev/tty1 exists (console=tty1 is on the
# cmdline). Start before initrd.target so the animation is already on screen while
# immucore mounts the real root. Torn down at switch-root, where the booted
# system's hadron-splash.service takes over.
DefaultDependencies=no
After=systemd-udev-trigger.service
Before=initrd.target
Conflicts=initrd-switch-root.target

[Service]
Type=simple
StandardInput=tty
StandardOutput=tty
TTYPath=/dev/tty1
TTYReset=yes
ExecStart=/usr/bin/hadron-splash
# hadron-splash self-limits to ~5s and restores the terminal on SIGTERM; switch-root
# will SIGTERM it if it is still running.
KillSignal=SIGTERM
TimeoutStopSec=5s
```

- [ ] **Step 6: Write the immucore quiet drop-in**

Create `rootfs/usr/lib/dracut/modules.d/50hadron-splash/immucore-quiet.conf`:

```ini
# Keep immucore's logs off the console so they don't print over the hadron-splash
# animation in the initramfs. immucore.service (from kairos-init's 28immucore
# module) defaults to StandardOutput=journal+console; this drop-in keeps the
# journal (so `journalctl` inside the initramfs still has everything) but drops
# the console sink. Delete this file from the 50hadron-splash dracut module to
# see immucore's mount progress on screen again.
[Service]
StandardOutput=journal
StandardError=journal
```

- [ ] **Step 7: Make the module setup script executable**

dracut sources `module-setup.sh`, but `require_binaries` and the check phase behave inconsistently if the mode bit is missing, and the test asserts it:

```bash
chmod +x rootfs/usr/lib/dracut/modules.d/50hadron-splash/module-setup.sh
```

- [ ] **Step 8: Cache-bust the splash and kairos-init stages**

This is the trap AIOS documented: buildx caches the kairos-init stage, whose `dracut -f` step bakes the splash into the initramfs, so the initramfs keeps shipping a stale splash even after `splash/main.c` changes. In the `Makefile`, change the `image` target from:

```make
image:
	docker build $(BUILD_ARGS) -t $(IMAGE) .
```

to:

```make
# --no-cache-filter on hadron-splash and kairos: the kairos stage runs
# kairos-init's `dracut -f`, which bakes the splash into /boot/initrd. BuildKit
# happily caches that step even when splash/main.c changed, silently shipping a
# stale initramfs splash. Busting both stages keeps the initramfs honest.
image:
	docker build $(BUILD_ARGS) --no-cache-filter hadron-splash,kairos -t $(IMAGE) .
```

- [ ] **Step 9: Rebuild and run the test to verify it passes**

```bash
make image
test/boot-ux/check-initramfs.sh sway-desktop:dev
```

Expected: seven `PASS` lines, exit 0.

If "splash baked into the initramfs" still fails while the overlay files pass, the `COPY --from=hadron-splash` from Task 1 Step 11 is landing after the kairos stage — move it earlier.

- [ ] **Step 10: Verify the splash actually paints in a VM**

The automated checks prove the bits are in place; only a boot proves the animation renders. The repo already has the correct QEMU invocation (UEFI + virtio-gpu — the default VGA renders the boot console as garbled static):

```bash
make iso
tools/vm.sh install
```

Expected: the animated HADRON wordmark appears on tty1 **during early boot, before the root filesystem is mounted**, and again before the login screen. Confirm no immucore mount messages print over the first one.

Note the live ISO boot is still noisy at this point — Task 4 adds the quiet cmdline that cleans it up. Judge only whether the animation renders.

- [ ] **Step 11: Commit**

```bash
git add rootfs/etc/dracut.conf.d/50-hadron-splash.conf \
        rootfs/usr/lib/dracut/modules.d/50hadron-splash/ \
        test/boot-ux/check-initramfs.sh Makefile
git commit -m "feat(splash): paint the splash from early initramfs

Add a 50hadron-splash dracut module that bakes /usr/bin/hadron-splash and an
initrd.target-ordered unit into the initramfs, plus an immucore drop-in that
takes its mount logs off the console. Cache-bust the splash and kairos stages
so a changed splash can't leave a stale initramfs behind."
```

---

### Task 3: Console font and per-variant GRUB theme subtitle

Two small branding corrections. The GRUB theme currently lies about which variant it is, and the console font may lack the glyphs the splash draws with.

**Files:**
- Create: `rootfs/etc/vconsole.conf`
- Modify: `rootfs/etc/kairos/branding/hadron-theme/theme.txt`
- Modify: `Dockerfile` (re-`COPY` vconsole.conf after kairos-init; substitute the subtitle)
- Test: `test/boot-ux/check-branding.sh`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `/etc/kairos/branding/hadron-theme/theme.txt` in the image, with its subtitle line reading `sway · wayland · kairos` or `i3 · xlibre · kairos` per the `DESKTOP` build arg. Task 4 copies this file verbatim into the ISO overlay.

- [ ] **Step 1: Write the failing test**

Create `test/boot-ux/check-branding.sh`:

```bash
#!/usr/bin/env bash
# Assert per-variant branding is correct in a built image.
#
#   test/boot-ux/check-branding.sh IMAGE EXPECTED_SUBTITLE
#
# e.g. test/boot-ux/check-branding.sh sway-desktop:dev 'sway · wayland · kairos'
#      test/boot-ux/check-branding.sh i3-desktop:dev   'i3 · xlibre · kairos'
#
# The subtitle check catches a real defect: theme.txt lives in the COMMON overlay
# but hardcoded the sway subtitle, so the i3 boot menu claimed to be sway.
set -euo pipefail
IMAGE="${1:-sway-desktop:dev}"
EXPECTED="${2:-sway · wayland · kairos}"
fail=0
check() {
    local desc="$1"; shift
    if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"; fail=1
    fi
}

T=/etc/kairos/branding/hadron-theme/theme.txt
check "vconsole.conf present"        'test -f /etc/vconsole.conf'
check "vconsole.conf sets a font"    'grep -q "^FONT=" /etc/vconsole.conf'
check "theme.txt present"            "test -f $T"
check "theme subtitle is '$EXPECTED'" "grep -qF '$EXPECTED' $T"
check "theme has no unsubstituted placeholder" "! grep -q '@VARIANT_SUBTITLE@' $T"

exit "$fail"
```

```bash
chmod +x test/boot-ux/check-branding.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
make DESKTOP=i3 image
test/boot-ux/check-branding.sh i3-desktop:dev 'i3 · xlibre · kairos'
```

Expected: `FAIL: vconsole.conf present`, `FAIL: vconsole.conf sets a font`, and `FAIL: theme subtitle is 'i3 · xlibre · kairos'` — the last one is the defect, since the common theme hardcodes the sway subtitle.

- [ ] **Step 3: Add the console font config**

Create `rootfs/etc/vconsole.conf`:

```
# Console font for the kernel VT. Chosen for glyph coverage: the boot splash
# draws its wordmark with block and box-drawing characters (█ ╗ ╔ ═ ║ ╚ ╝), and
# the installer draws separators with them too. The kernel's built-in font has
# gaps there and renders them as blanks or garbage.
#
# kairos-init may regenerate this file, so the Dockerfile re-COPYs it after the
# kairos-init stage — same treatment as /etc/issue and /etc/motd.
FONT=Lat2-Terminus16
```

- [ ] **Step 4: Parameterize the theme subtitle**

In `rootfs/etc/kairos/branding/hadron-theme/theme.txt`, replace the hardcoded subtitle line:

```
    text  = "sway · wayland · kairos"
```

with the placeholder:

```
    text  = "@VARIANT_SUBTITLE@"
```

Then extend the file's header comment — after the existing paragraph about `desktop-image` being resolved relative to this file — with:

```
# The subtitle is a build-time placeholder: this file lives in the COMMON rootfs
# overlay but the subtitle is per-variant, so the Dockerfile substitutes
# @VARIANT_SUBTITLE@ from /etc/hadron-desktop/session after kairos-init. Before
# that substitution existed the i3 variant's boot menu claimed to be sway.
```

- [ ] **Step 5: Substitute the subtitle and re-COPY vconsole.conf**

In the `kairos` stage of the `Dockerfile`, immediately after the existing `COPY rootfs/etc/kairos/branding/grubmenu.cfg /etc/kairos/branding/grubmenu.cfg` line, add:

```dockerfile
# Console font. kairos-init may regenerate /etc/vconsole.conf, so re-apply ours
# after it — same treatment as /etc/issue and /etc/motd below.
COPY rootfs/etc/vconsole.conf /etc/vconsole.conf
# Per-variant GRUB theme subtitle. theme.txt ships in the COMMON overlay with an
# @VARIANT_SUBTITLE@ placeholder; the variant overlay supplies DESKTOP_NAME and
# DISPLAY_NAME via /etc/hadron-desktop/session. Lowercased to match the theme's
# typographic style ("i3 · xlibre · kairos").
RUN . /etc/hadron-desktop/session && \
    subtitle="$(printf '%s · %s · kairos' "$DESKTOP_NAME" "$DISPLAY_NAME" | tr '[:upper:]' '[:lower:]')" && \
    sed -i "s|@VARIANT_SUBTITLE@|${subtitle}|" /etc/kairos/branding/hadron-theme/theme.txt && \
    grep -q '@VARIANT_SUBTITLE@' /etc/kairos/branding/hadron-theme/theme.txt && exit 1 || true
```

The trailing `grep ... && exit 1 || true` fails the build if the substitution silently didn't apply — a theme with a literal `@VARIANT_SUBTITLE@` would otherwise render that text on the boot menu.

- [ ] **Step 6: Rebuild both variants and run the test to verify it passes**

```bash
make DESKTOP=i3 image
test/boot-ux/check-branding.sh i3-desktop:dev 'i3 · xlibre · kairos'
make DESKTOP=sway image
test/boot-ux/check-branding.sh sway-desktop:dev 'sway · wayland · kairos'
```

Expected: five `PASS` lines from each invocation, exit 0 both times.

- [ ] **Step 7: Commit**

```bash
git add rootfs/etc/vconsole.conf \
        rootfs/etc/kairos/branding/hadron-theme/theme.txt \
        test/boot-ux/check-branding.sh Dockerfile
git commit -m "fix(branding): per-variant GRUB subtitle, add console font

theme.txt lives in the common overlay but hardcoded the sway subtitle, so the
i3 boot menu claimed to be sway; substitute it from /etc/hadron-desktop/session
at build time. Add vconsole.conf with Lat2-Terminus16 for the block and
box-drawing glyphs the splash and installer draw with."
```

---

### Task 4: Branded live ISO boot menu and quiet cmdline

Give the live ISO our own GRUB menu, themed and retitled, and put the quiet cmdline on every non-debug entry. This is what makes Tasks 1 and 2 pay off on the installer boot.

**Files:**
- Create: `auroraboot/live-grub.cfg.tmpl`
- Create: `auroraboot/build.sh`
- Modify: `Makefile` (`iso` and `agent-iso` route through `build.sh`; `clean`)
- Test: `test/boot-ux/check-iso-overlay.sh`

**Interfaces:**
- Consumes: `rootfs/etc/kairos/branding/hadron-theme/{theme.txt,unicode.pf2,background.tga}` — `theme.txt` as modified by Task 3, read from the **repo working tree**, not from the image.
- Produces: `auroraboot/build.sh`, invoked as `build.sh <IMAGE> <OUT_DIR> [ISO_NAME]`, honouring `ARCH` (default `amd64`), `AURORA_IMAGE`, and `HADRON_EXTRA_LIVE_CMDLINE`. It stages `<OUT_DIR>/iso-overlay/` and leaves the finished ISO in `<OUT_DIR>`.

- [ ] **Step 1: Write the failing test**

Create `test/boot-ux/check-iso-overlay.sh`. This tests the *staged overlay directory*, not the ISO — building an ISO takes minutes, and every failure mode worth catching here is visible in the staged files.

```bash
#!/usr/bin/env bash
# Assert the ISO overlay directory is staged correctly.
#
#   test/boot-ux/check-iso-overlay.sh [OVERLAY_DIR]
#
# Run after auroraboot/build.sh has staged the overlay (STAGE_ONLY=1 stages and
# exits without building an ISO). The placeholder check is the important one:
# AuroraBoot does NOT expand {{NOMODESET}}/{{EXTEND_CMDLINE}} in an OVERLAID
# file, only in its own default, so an unrendered placeholder would go straight
# onto the kernel cmdline as a literal.
set -euo pipefail
DIR="${1:-build/sway-desktop/iso/iso-overlay}"
CFG="$DIR/boot/grub2/grub.cfg"
QUIET='quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false'
fail=0
check() {
    local desc="$1"; shift
    if eval "$*" >/dev/null 2>&1; then echo "PASS: $desc"; else echo "FAIL: $desc"; fail=1; fi
}

check "grub.cfg staged"                 "test -f '$CFG'"
check "theme.txt staged"                "test -f '$DIR/boot/grub2/themes/hadron/theme.txt'"
check "font staged"                     "test -f '$DIR/boot/grub2/themes/hadron/unicode.pf2'"
check "background staged"               "test -f '$DIR/boot/grub2/themes/hadron/background.tga'"
check "no unrendered placeholders"      "! grep -q '{{' '$CFG'"
check "menu is retitled to hadron"      "grep -q 'menuentry \"hadron-desktop\"' '$CFG'"
check "no stale Kairos titles"          "! grep -q 'menuentry \"Kairos' '$CFG'"
check "default entry carries install-mode" \
    "grep -q 'menuentry \"hadron-desktop\"' '$CFG' && grep -q 'install-mode' '$CFG'"
check "quiet cmdline on the default entry" "grep -qF '$QUIET' '$CFG'"
check "debug entry left verbose" \
    "awk '/menuentry \"hadron-desktop \\(debug\\)\"/,/^}/' '$CFG' | grep -q 'rd.debug' && ! awk '/menuentry \"hadron-desktop \\(debug\\)\"/,/^}/' '$CFG' | grep -q 'quiet'"
check "theme load is guarded"           "grep -q 'if \[ -f (\$root)/boot/grub2/themes/hadron/theme.txt \]' '$CFG'"

exit "$fail"
```

```bash
chmod +x test/boot-ux/check-iso-overlay.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/boot-ux/check-iso-overlay.sh
```

Expected: `FAIL` on every check — the overlay directory does not exist yet.

- [ ] **Step 3: Write the live GRUB menu template**

Create `auroraboot/live-grub.cfg.tmpl`. This is a snapshot of AuroraBoot's embedded `pkg/constants/grub_live_bios.cfg` with entries retitled and the quiet cmdline added to the normal boot paths.

```
# hadron-desktop LiveCD GRUB menu — overrides AuroraBoot's hardcoded "Kairos" menu.
#
# AuroraBoot embeds pkg/constants/grub_live_bios.cfg and writes it to the ISO at
# /boot/grub2/grub.cfg — but ONLY if that path does not already exist. We ship our
# own via `--overlay-iso` (auroraboot/build.sh), so AuroraBoot keeps this one and
# the live boot menu reads "hadron-desktop" rather than "Kairos". `--override-name`
# only renames the output .iso file, not the menu.
#
# This is a snapshot of upstream grub_live_bios.cfg with three changes:
#   1. menuentry titles "Kairos*" -> "hadron-desktop*"
#   2. the Tokyo Night gfxterm theme loaded from the ISO
#   3. the quiet cmdline added to the normal boot paths (recovery/debug kept verbose)
# Keep `CDLABEL=COS_LIVE`, the ($root)/boot/kernel + initrd paths, and the
# {{NOMODESET}}/{{EXTEND_CMDLINE}} placeholders intact. AuroraBoot does NOT expand
# those placeholders for an overlaid file (it only substitutes its own default), so
# build.sh renders them per-arch before handing this to AuroraBoot. Re-sync from
# https://github.com/kairos-io/AuroraBoot/blob/main/pkg/constants/grub_live_bios.cfg
# if upstream changes the entries or live cmdline.
search --file --set=root /boot/kernel
set default=0
set timeout=10
set timeout_style=menu

set font=($root)/boot/${grub_cpu}/loader/grub2/fonts/unicode.pf2
if [ -f ${font} ];then
    loadfont ${font}
fi

# Tokyo Night theme (gfxterm). We load the theme's own vendored unicode.pf2 (the
# name theme.txt references, "Unifont Regular 16") plus the TGA background, so the
# live and installed menus look identical. The background is TGA because GRUB's
# png.mod rejects valid PNGs with "unsupported format". Best-effort: if video, font
# or theme aren't available GRUB keeps the plain text menu, so a failure here can
# never block boot.
insmod all_video
insmod gfxterm
insmod tga
if [ -f ($root)/boot/grub2/themes/hadron/theme.txt ]; then
    loadfont ($root)/boot/grub2/themes/hadron/unicode.pf2
    set gfxmode=auto
    terminal_output gfxterm
    set theme=($root)/boot/grub2/themes/hadron/theme.txt
fi

# Get model
smbios --type 4 --get-string 5 --set model
# For Thor we need to set the ignored clk and pd, otherwise devices will die during boot
if test $model == "Thor"; then
  echo "Thor device detected, setting ignored clocks and power domains and proper console output"
  set thor_options="pd_ignore_unused clk_ignore_unused console=ttyUTC0,115200 earlycon=tegra_utc,mmio32,0xc5a0000"
fi

# Add nomodeset only on x86/amd64-class arches.
# Template placeholders: {{NOMODESET}} and {{EXTEND_CMDLINE}} are replaced at build time.
menuentry "hadron-desktop" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable vga=795{{NOMODESET}} install-mode selinux=0 $thor_options rd.live.overlay.overlayfs quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

menuentry "hadron-desktop (manual)" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable vga=795{{NOMODESET}} selinux=0 $thor_options rd.live.overlay.overlayfs quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

menuentry "hadron-desktop (interactive install)" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable vga=795{{NOMODESET}} install-mode-interactive selinux=0 $thor_options rd.live.overlay.overlayfs quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

menuentry "hadron-desktop (remote recovery mode)" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 rd.cos.disable vga=795{{NOMODESET}} kairos.remote_recovery_mode selinux=0 $thor_options rd.live.overlay.overlayfs{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

menuentry "hadron-desktop (boot local node from livecd)" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=ttyS0 console=tty1 kairos.boot_live_mode vga=795{{NOMODESET}} selinux=0 $thor_options rd.live.overlay.overlayfs quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

menuentry "hadron-desktop (debug)" --class os --unrestricted {
    echo Loading kernel...
    linux ($root)/boot/kernel cdroot root=live:CDLABEL=COS_LIVE rd.live.dir=/ rd.live.squashimg=rootfs.squashfs net.ifnames=1 console=tty0 rd.debug rd.shell rd.cos.disable rd.immucore.debug vga=795{{NOMODESET}} selinux=0 $thor_options rd.live.overlay.overlayfs{{EXTEND_CMDLINE}}
    echo Loading initrd...
    initrd ($root)/boot/initrd
}

if [ "${grub_platform}" = "efi" ]; then
    hiddenentry "Text mode" --hotkey "t" {
        set textmode=true
        terminal_output console
    }
fi
```

- [ ] **Step 4: Write the ISO build script**

Create `auroraboot/build.sh`. Unlike AIOS's equivalent, this needs no local-registry dance: the existing Makefile already hands AuroraBoot a `docker:` reference over the mounted Docker socket, and that keeps working.

```bash
#!/usr/bin/env bash
# Build a branded installer ISO with AuroraBoot.
#
#   auroraboot/build.sh <IMAGE> <OUT_DIR> [ISO_NAME]
#
# e.g. auroraboot/build.sh sway-desktop:dev build/sway-desktop/iso
#
# Env:
#   ARCH                        target arch (default amd64)
#   AURORA_IMAGE                AuroraBoot image (default matches the Makefile pin)
#   HADRON_EXTRA_LIVE_CMDLINE   extra args appended to every live menuentry
#   STAGE_ONLY=1                stage the overlay dir and exit (used by tests)
#
# AuroraBoot reads the source image straight from the host Docker daemon via the
# mounted socket (`docker:` reference), so no intermediate registry is needed.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${1:?usage: build.sh <IMAGE> <OUT_DIR> [ISO_NAME]}"
OUT_DIR="${2:?usage: build.sh <IMAGE> <OUT_DIR> [ISO_NAME]}"
ISO_NAME="${3:-}"
ARCH="${ARCH:-amd64}"
AURORA_IMAGE="${AURORA_IMAGE:-quay.io/kairos/auroraboot:v0.21.0-alpha.4}"

THEME_SRC="rootfs/etc/kairos/branding/hadron-theme"
OVERLAY_DIR="$OUT_DIR/iso-overlay"

mkdir -p "$OUT_DIR"

# --- stage the --overlay-iso directory -------------------------------------
# AuroraBoot's build-iso only writes its default /boot/grub2/grub.cfg (the
# hardcoded "Kairos" menu) when that file does not already exist, so overlaying
# our own makes it keep ours. AuroraBoot substitutes {{NOMODESET}} and
# {{EXTEND_CMDLINE}} only on its own default and NOT on an overlaid file, so we
# reproduce that substitution here per-arch (see AuroraBoot pkg/ops/iso.go
# prepareBootArtifacts: amd64/x86 -> " nomodeset").
echo "==> staging live GRUB menu + theme (arch=$ARCH)"
case "$ARCH" in
  amd64|x86_64) NOMODESET=" nomodeset" ;;
  *)            NOMODESET="" ;;
esac
EXTEND_CMDLINE="${HADRON_EXTRA_LIVE_CMDLINE:+ ${HADRON_EXTRA_LIVE_CMDLINE}}"

rm -rf "$OVERLAY_DIR"
mkdir -p "$OVERLAY_DIR/boot/grub2/themes/hadron"
sed -e "s|{{NOMODESET}}|${NOMODESET}|g" \
    -e "s|{{EXTEND_CMDLINE}}|${EXTEND_CMDLINE}|g" \
    auroraboot/live-grub.cfg.tmpl > "$OVERLAY_DIR/boot/grub2/grub.cfg"

# Ship the Tokyo Night theme alongside the menu so the LIVE boot is themed too.
# Same three files the installed system gets from the 09_hadron_grub_theme oem
# stage — one source of truth for both. The font must travel with the theme:
# theme.txt references "Unifont Regular 16", which only exists once our vendored
# unicode.pf2 is loaded. The background is TGA because GRUB's png.mod rejects
# valid PNGs with "unsupported format".
cp "$THEME_SRC/theme.txt" "$THEME_SRC/unicode.pf2" "$THEME_SRC/background.tga" \
   "$OVERLAY_DIR/boot/grub2/themes/hadron/"

# theme.txt carries an @VARIANT_SUBTITLE@ placeholder that the Dockerfile
# substitutes inside the image. The copy we ship on the ISO comes from the repo
# working tree, so substitute it here too — an unrendered placeholder would be
# drawn literally on the boot menu. Default to the sway wording unless the image
# tag says otherwise.
case "$IMAGE" in
  i3-*|*/i3-*)   SUBTITLE="i3 · xlibre · kairos" ;;
  agent-*|*/agent-*) SUBTITLE="agent · xlibre · kairos" ;;
  *)             SUBTITLE="sway · wayland · kairos" ;;
esac
sed -i "s|@VARIANT_SUBTITLE@|${SUBTITLE}|" "$OVERLAY_DIR/boot/grub2/themes/hadron/theme.txt"

if [ "${STAGE_ONLY:-0}" = "1" ]; then
  echo "==> staged $OVERLAY_DIR (STAGE_ONLY=1, not building an ISO)"
  exit 0
fi

# --- build --------------------------------------------------------------
rm -f "$OUT_DIR"/*.iso
echo "==> building ISO with AuroraBoot (source: docker:$IMAGE)"
docker run --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/$OUT_DIR:/output" \
  -v "$PWD/$OVERLAY_DIR:/overlay:ro" \
  "$AURORA_IMAGE" build-iso \
    --output /output/ \
    --overlay-iso /overlay \
    ${ISO_NAME:+--override-name "$ISO_NAME"} \
    "docker:$IMAGE"

ISO="$(ls -t "$OUT_DIR"/*.iso 2>/dev/null | head -1)"
[ -n "$ISO" ] || { echo "no ISO produced"; exit 1; }
echo "==> $ISO"
ls -lh "$ISO"
```

```bash
chmod +x auroraboot/build.sh
```

- [ ] **Step 5: Stage the overlay and run the test to verify it passes**

```bash
STAGE_ONLY=1 auroraboot/build.sh sway-desktop:dev build/sway-desktop/iso
test/boot-ux/check-iso-overlay.sh build/sway-desktop/iso/iso-overlay
```

Expected: eleven `PASS` lines, exit 0.

- [ ] **Step 6: Route the Makefile ISO targets through the script**

Replace the `iso` target:

```make
# Build the installer ISO with AuroraBoot via auroraboot/build.sh, which stages
# an --overlay-iso dir carrying our own live GRUB menu (branded, themed, quiet
# cmdline) so AuroraBoot keeps ours instead of writing its default Kairos menu.
iso: image
	AURORA_IMAGE=$(AURORA_IMAGE) auroraboot/build.sh $(IMAGE) $(ISO_DIR)
```

and the `agent-iso` target:

```make
agent-iso: agent-image
	AURORA_IMAGE=$(AURORA_IMAGE) auroraboot/build.sh $(AGENT_IMAGE) $(AGENT_ISO_DIR)
```

Both previously inlined `docker run ... $(AURORA_IMAGE) build-iso`, created their output dir, and `rm -f`'d stale ISOs — `build.sh` now does all three.

- [ ] **Step 7: Build the ISO end to end**

```bash
make iso
```

Expected: the `==> staging live GRUB menu + theme (arch=amd64)` line, then AuroraBoot output, then a `==> build/sway-desktop/iso/....iso` line with a size.

- [ ] **Step 8: Verify the live boot in a VM**

```bash
tools/vm.sh install
```

Expected, in order:
1. A **themed** GRUB menu — Tokyo Night background, `hadron-desktop` entries, no "Kairos" anywhere, and the subtitle matching the variant.
2. On selecting the default entry: the animated HADRON splash **with a clean screen** — no udev or systemd status churn over it. This is the payoff from Tasks 1, 2 and 4 together.
3. The installer taking over tty1.

If the menu is plain text, the theme guard failed — check that all three theme files landed in `iso-overlay/boot/grub2/themes/hadron/`.

- [ ] **Step 9: Update clean and commit**

Change the `clean` target to remove the staged overlay along with the rest (the overlay lives under `$(WORK)`/`$(AGENT_WORK)` already, so verify rather than assume):

```bash
make clean && test ! -d build/sway-desktop/iso/iso-overlay && echo "clean OK"
```

Expected: `clean OK`. If the directory survives, add its removal to the `clean` target.

```bash
git add auroraboot/ test/boot-ux/check-iso-overlay.sh Makefile
git commit -m "feat(iso): branded, themed, quiet live boot menu

Stage an --overlay-iso dir with our own live grub.cfg so AuroraBoot keeps it
instead of writing the default Kairos menu: hadron-desktop entries, the Tokyo
Night theme, and the full quiet cmdline on every non-debug entry so the
initramfs splash paints on a clean screen. Route iso/agent-iso through it."
```

---

### Task 5: Installed-system cmdline parity

The installed system currently sets only `quiet loglevel=3`. Bring it up to the same argument set as the live boot so both behave identically.

**Files:**
- Modify: `rootfs/etc/kairos/branding/grubmenu.cfg`
- Test: `test/boot-ux/check-branding.sh` (extend)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: nothing later tasks depend on. This is the last task in the plan.

- [ ] **Step 1: Extend the test**

In `test/boot-ux/check-branding.sh`, add these two checks immediately before the final `exit "$fail"`:

```bash
G=/etc/kairos/branding/grubmenu.cfg
check "grubmenu.cfg sets the full quiet cmdline" \
    "grep -qF 'quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false' $G"
check "grubmenu.cfg preserves any pre-existing extra_cmdline" \
    "grep -q 'set extra_cmdline=\"\${extra_cmdline}' $G"
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/boot-ux/check-branding.sh sway-desktop:dev 'sway · wayland · kairos'
```

Expected: `FAIL: grubmenu.cfg sets the full quiet cmdline`. The second new check passes already — the existing line does preserve `${extra_cmdline}`, and asserting it guards against the edit in Step 3 dropping it.

- [ ] **Step 3: Extend the installed-system cmdline**

In `rootfs/etc/kairos/branding/grubmenu.cfg`, replace this line:

```
set extra_cmdline="${extra_cmdline} quiet loglevel=3"
```

with:

```
set extra_cmdline="${extra_cmdline} quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false"
```

Then update the comment block directly above it. Replace the sentence beginning "`quiet loglevel=3` drops that message churn..." with:

```
# `quiet splash loglevel=3` drops that message churn, and udev.log_level=3 plus
# the two show_status=false flags silence udev and systemd's own progress output,
# so the initramfs splash (50hadron-splash dracut module) and the late splash both
# paint on a clean screen. This is the SAME argument set the live ISO menu uses
# (auroraboot/live-grub.cfg.tmpl) so live and installed boots look identical.
# Drop `quiet` here to debug a boot. extra_cmdline is appended to every boot
# entry's kernel cmdline; we preserve any pre-existing value (e.g. from grubenv)
# and add to it.
```

- [ ] **Step 4: Rebuild and run the test to verify it passes**

```bash
make image
test/boot-ux/check-branding.sh sway-desktop:dev 'sway · wayland · kairos'
```

Expected: seven `PASS` lines, exit 0.

- [ ] **Step 5: Verify an installed boot**

The prior VM checks covered the live ISO. This one covers the installed system, which uses a different GRUB config path entirely (`/etc/cos/grub.cfg` sourcing `grubmenu` from COS_STATE).

```bash
tools/vm.sh install    # complete the install, let it reboot
tools/vm.sh run        # boot the installed disk
```

Expected: themed GRUB menu, then the splash on a clean screen from early boot through to the ly login, with no visible boot-message churn between the two splash phases.

- [ ] **Step 6: Add a test runner entrypoint**

Create `test/boot-ux/run.sh` so the four checks can be run as one command:

```bash
#!/usr/bin/env bash
# Run all boot-UX checks against a built image.
#
#   test/boot-ux/run.sh [IMAGE] [SUBTITLE]
#
# Stages the ISO overlay (without building an ISO) so the overlay checks can run
# cheaply. Does not boot a VM — see the plan's manual verification steps for that.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

IMAGE="${1:-sway-desktop:dev}"
SUBTITLE="${2:-sway · wayland · kairos}"
OVERLAY_PARENT="build/boot-ux"
fail=0

"$SCRIPT_DIR/check-splash.sh"    "$IMAGE" || fail=1
"$SCRIPT_DIR/check-initramfs.sh" "$IMAGE" || fail=1
"$SCRIPT_DIR/check-branding.sh"  "$IMAGE" "$SUBTITLE" || fail=1

STAGE_ONLY=1 auroraboot/build.sh "$IMAGE" "$OVERLAY_PARENT" >/dev/null
"$SCRIPT_DIR/check-iso-overlay.sh" "$OVERLAY_PARENT/iso-overlay" || fail=1

if [ "$fail" -eq 0 ]; then echo "boot-ux: ALL PASS"; else echo "boot-ux: FAILURES"; fi
exit "$fail"
```

```bash
chmod +x test/boot-ux/run.sh
test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
```

Expected: all checks pass, final line `boot-ux: ALL PASS`, exit 0.

- [ ] **Step 7: Document the checks**

Create `test/boot-ux/README.md`:

```markdown
# Boot UX checks

Fast, non-booting assertions for the splash, initramfs, branding and ISO
overlay. Run against an already-built image:

    make image
    test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
    test/boot-ux/run.sh i3-desktop:dev   'i3 · xlibre · kairos'

| Script | Asserts |
|---|---|
| `check-splash.sh` | `/usr/bin/hadron-splash` present, non-tty fallback prints `HADRON`, no stale branding |
| `check-initramfs.sh` | dracut module files present **and** the splash actually baked into `/boot/initrd` |
| `check-branding.sh` | `vconsole.conf`, per-variant GRUB theme subtitle, full quiet cmdline in `grubmenu.cfg` |
| `check-iso-overlay.sh` | staged live `grub.cfg` is retitled, themed, quiet, and has no unrendered `{{...}}` placeholders |

These do not boot anything. The animation itself, and the themed GRUB menu, need
a VM:

    make iso && tools/vm.sh install   # live ISO: menu, then splash, then installer
    tools/vm.sh run                   # installed disk: menu, then splash, then ly

## Why the initramfs check matters

Two failure modes leave every other check passing while the feature silently
does nothing:

1. The splash binary landing in the image **after** kairos-init runs `dracut -f`.
2. BuildKit **caching** the kairos stage, shipping a stale initramfs.

`check-initramfs.sh` inspects `/boot/initrd` itself, which is the only way to
catch either.
```

- [ ] **Step 8: Commit**

```bash
git add rootfs/etc/kairos/branding/grubmenu.cfg \
        test/boot-ux/check-branding.sh test/boot-ux/run.sh test/boot-ux/README.md
git commit -m "feat(boot): installed-system cmdline parity with the live boot

grubmenu.cfg set only 'quiet loglevel=3'; add splash, udev.log_level and the two
show_status flags so the installed boot is as quiet as the live one and both
splash phases paint on a clean screen. Add a boot-ux check runner."
```

---

## Verification checklist

After all five tasks:

```bash
make image      && test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
make DESKTOP=i3 image && test/boot-ux/run.sh i3-desktop:dev 'i3 · xlibre · kairos'
make iso        && tools/vm.sh install
tools/vm.sh run
```

The VM runs are the real test. Both boots should show: themed GRUB menu with the
correct variant subtitle → animated splash from early boot on a clean screen →
(live) installer, or (installed) a second splash phase then the ly login.

## Open item carried from the spec

The spec flags one assumption that has **not** been verified against our pinned
AuroraBoot: that `build-iso` writes its default `/boot/grub2/grub.cfg` only when
that path does not already exist. This is documented from AIOS, which pins
`quay.io/kairos/auroraboot:latest`; we pin `v0.21.0-alpha.4`.

Task 4 Step 8 is where this surfaces — if the live menu still reads "Kairos"
after a successful build, AuroraBoot overwrote our overlaid file and the
`--overlay-iso` approach needs revisiting for this version (most likely by
bumping `AURORA_IMAGE`, which is deliberately out of scope for this plan).
