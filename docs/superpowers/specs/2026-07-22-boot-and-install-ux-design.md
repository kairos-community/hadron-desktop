# Boot and install UX

Date: 2026-07-22
Status: approved, not yet implemented

Bring hadron-desktop's boot and install experience up to the standard set by
[AIOS](https://github.com/mudler/AIOS): a customized splash that covers the boot
from early initramfs through to the login screen, a branded live ISO boot menu,
and a real installer TUI instead of raw `kairos-agent` log output.

## Motivation

Today the first-time experience has four visible gaps:

1. **Nothing renders between GRUB and the login screen.** `hadron-splash` only
   runs late, ordered `Before=ly@tty1.service`. The initramfs, switch-root and
   early boot are a blank or scrolling console.
2. **The splash is stock upstream.** hadron-desktop ships the base image's
   generic `hadron-splash` unmodified; there is no hadron-desktop identity in it.
3. **The live ISO boot menu is unbranded Kairos.** `make iso` invokes AuroraBoot
   with no overlay and no cloud-config, so the very first screen a user sees is
   generic — and there is nowhere to set the live kernel cmdline.
4. **The install is a wall of logs.** `hadron-install` collects answers with
   POSIX-sh prompts, then `exec`s `kairos-agent install`, whose raw output takes
   over the screen for the entire install.

AIOS solved all four. Its `aios-splash` is itself a rebrand of the same upstream
`hadron-splash` this repo already ships, so the splash work is a fork-and-retheme
rather than new invention.

## Scope

In scope: splash (initramfs + rootfs), GRUB theming for the live ISO, kernel
cmdline quieting, and a Go installer TUI for the desktop path.

Out of scope: the agent appliance's installer logic. `hadron-agent-install`
carries an explicit safety contract — it never wipes a disk unless a human typed
`yes` or a seed was re-verified via `hadron-agent provision inspect-seed
--require-auto-install`. That code and its test seams are not being rewritten.
The appliance gains the branded progress screen only, via a one-line default
change.

## Part 1 — Splash, initramfs through login

### Vendored splash source

New `splash/` directory holding a C99 fork of the upstream splash (by way of
AIOS's `image/aios-splash/main.c`), rebranded for hadron-desktop:

- Tokyo Night palette expressed in **VGA-16 colours only** — `4` blue, `12`
  bright blue, `14` cyan, `15` white-hot. The kernel VT is a 16-colour console;
  256-colour or truecolor Tokyo Night values round to the wrong hue there. This
  is the same constraint AIOS documents for its amber ramp.
- "HADRON" ANSI-Shadow wordmark, desktop-appropriate boot lines replacing AIOS's
  cluster-flavoured ones.
- Unchanged from upstream: ~5s duration at ~30fps, `SIGTERM` handling, and the
  non-tty / too-small-terminal fallback to a plain `puts`.

Built in a `FROM toolchain AS hadron-splash` stage (the repo already uses
`ghcr.io/kairos-io/hadron-toolchain:main AS toolchain`) with the size-optimized
flags AIOS uses (`-Os -std=c99 -flto -Wl,--gc-sections -s`), plus a build-time
smoke test (`./hadron-splash | grep -q HADRON`) that exercises the non-tty path.

Output overwrites `/usr/bin/hadron-splash`, replacing the base image's stock
binary, so the initramfs and rootfs copies come from one source.

### Initramfs coverage

Three new overlay files under `rootfs/`:

| Path | Purpose |
|---|---|
| `etc/dracut.conf.d/50-hadron-splash.conf` | `add_dracutmodules+=" hadron-splash "` |
| `usr/lib/dracut/modules.d/50hadron-splash/module-setup.sh` | installs binary + unit into the initramfs |
| `usr/lib/dracut/modules.d/50hadron-splash/hadron-splash.service` | the initramfs-side unit |

`module-setup.sh` uses `inst_multiple` rather than `inst_simple` — the splash is
a **dynamic musl binary** and needs its shared-library dependencies pulled in —
and creates an `initrd.target.requires` symlink. Modeled on kairos-init's own
`28immucore` module.

The initramfs unit:

```ini
[Unit]
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
KillSignal=SIGTERM
TimeoutStopSec=5s
```

The same module installs `immucore.service.d/10-quiet.conf` setting
`StandardOutput=journal` / `StandardError=journal`. Immucore defaults to
`journal+console`; without this its mount progress prints over the animation.
Deleting the file is the documented way to debug a boot.

### Build ordering

Two constraints, both of which fail silently if violated:

1. The `COPY` of the splash binary must land in the rootfs stage **before** the
   kairos-init stage runs `dracut -f`, or the dracut module has nothing to
   install.
2. The kairos-init stage must be **cache-busted** (`--no-cache-filter` on both
   the splash stage and the kairos-init stage). AIOS hit exactly this: buildx
   cached the dracut step and kept shipping a stale initramfs splash after the
   binary changed.

### Handoff chain

```
GRUB (themed)
  → initramfs hadron-splash.service (tty1, alt-screen)
  → switch-root SIGTERMs it; program restores the terminal
  → rootfs hadron-splash.service (Before=ly@tty1.service)
  → ly login
```

The existing rootfs unit and its live-mode guards are unchanged. hadron-desktop
keeps `Before=ly@tty1.service` rather than AIOS's `Before=getty.target`, because
ly owns tty1 here.

Seamlessness depends on three mechanisms stacking:

- **Alternate screen buffer** (`\033[?1049h` / `\033[?1049l` + `\033[3J`) rather
  than a Plymouth-style retain-splash, so the primary buffer and scrollback are
  clean for switch-root and for ly.
- **Differential rendering with a forced full repaint every 15 frames**, so a
  stray kernel or udev write onto tty1 gets painted over by our own cells within
  half a second — without the flicker a per-frame `clear` would cause.
- **The quiet cmdline** (Part 2), which keeps the gap between the two splash runs
  black instead of a wall of boot logs.

## Part 2 — GRUB theming and ISO restructure

### Theme subtitle defect

`rootfs/etc/kairos/branding/hadron-theme/theme.txt` lives in the *common* overlay
but hardcodes `"sway · wayland · kairos"`, so the i3 variant's boot menu
currently claims to be sway. The subtitle becomes build-time substituted from the
existing `DESKTOP` arg, consistent with the `/etc/hadron-desktop/session` values
the installer already reads.

### Live ISO menu

New `auroraboot/live-grub.cfg.tmpl` and `auroraboot/build.sh`.

AuroraBoot writes its embedded live `grub.cfg` to the ISO **only if that path
does not already exist**. Shipping our own via `--overlay-iso` therefore makes it
back off. (`--override-name` only renames the output `.iso` file, not the menu —
this is why it never appeared to take effect.)

The template retitles every entry to hadron-desktop and loads the theme
best-effort, so a missing asset degrades to the plain text menu rather than
blocking boot — the same discipline already used in `grubmenu.cfg`:

```
insmod all_video
insmod gfxterm
if [ -f ($root)/boot/grub2/themes/hadron/theme.txt ]; then
    loadfont ($root)/boot/grub2/themes/hadron/unicode.pf2
    set gfxmode=auto
    terminal_output gfxterm
    set theme=($root)/boot/grub2/themes/hadron/theme.txt
fi
```

`build.sh` stages an overlay directory:

- renders `{{NOMODESET}}` and `{{EXTEND_CMDLINE}}` **itself** with `sed`.
  AuroraBoot does not expand its placeholders in overlaid files; skipping this
  ships the literal `{{NOMODESET}}` onto the kernel cmdline.
- copies `theme.txt`, `unicode.pf2` and `background.tga` into
  `/boot/grub2/themes/hadron/`.
- invokes AuroraBoot with `--overlay-iso`.

### Quiet boot

Every non-debug live entry gains:

```
quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false
```

This is what makes Part 1 pay off on the live/installer boot — without it the
initramfs splash is scribbled over by udev and systemd status output. Recovery
and debug entries stay deliberately verbose.

The installed system keeps its current mechanism (`grubmenu.cfg` sets
`extra_cmdline`; `09_hadron_grub_theme.yaml` pushes assets to COS_STATE),
extended with the arguments it currently lacks (`splash`, `udev.log_level=3`,
`systemd.show_status=false`, `rd.systemd.show_status=false` — it sets only
`quiet loglevel=3` today) so live and installed behave identically.

### Makefile

`iso` and `agent-iso` both route through `auroraboot/build.sh` instead of calling
`docker run auroraboot` inline, so the appliance ISO gets the same branded menu.
`clean` learns about the staging directory.

### Console font

New `rootfs/etc/vconsole.conf` with `FONT=Lat2-Terminus16` for box-drawing glyph
coverage, carrying the post-kairos-init re-`COPY` the other branding files
already use — kairos-init clobbers `/etc/issue`, `/etc/motd`,
`/etc/vconsole.conf` and `/etc/kairos/branding/grubmenu.cfg`.

## Part 3 — Installer

### New Go module

`install-ui/` builds `/usr/bin/hadron-install-ui` in a
`FROM golang:1.25.0-alpine3.22 AS install-ui` stage with `CGO_ENABLED=0`, mirroring
what `Dockerfile.agent` already does. A static Go binary runs on musl without
issue. Binary lives in `/usr/bin`, not `/usr/local/bin` — AIOS documents that
`/usr/local` is shadowed by an empty overlay at runtime.

Two modes:

- default — wizard, then progress (desktop path)
- `--progress-only` — skip prompts, wrap the install, render progress

### Wizard

bubbletea, alternate screen. Steps: hostname → username → password → GitHub SSH
keys → disk → **review**. Esc navigates back everywhere except step 1, with
inputs re-seeded from already-collected state so values survive back-navigation.
The review screen leads with the target device in red and requires an explicit
confirm before anything irreversible.

Contracts preserved verbatim from `hadron-install`, because they are test and
automation seams:

- `config_present()` — an existing user/install config in `/oem` skips the
  prompts and installs unattended
- `HADRON_INSTALL_NONINTERACTIVE=1` with `HADRON_USER`, `HADRON_PASS`,
  `HADRON_DISK`, `HADRON_HOSTNAME`, `HADRON_GITHUB`
- `HADRON_INSTALL_CMD`, `HADRON_OEM_DIR`
- byte-identical `/oem/99_hadron-user.yaml`: install block, hostname, user with
  the seven desktop groups, `ssh_authorized_keys: github:`, and the
  `/etc/ly/save.ini` stage

**Password hashing keeps shelling out to `openssl passwd -6 -stdin`.** Go's
stdlib has no `crypt(3)`, and calling the binary already present in the image
beats vendoring a SHA-512-crypt implementation into the one code path where a
mistake locks the user out of their machine.

### Progress screen

`kairos-agent install` exposes no progress API, so progress is derived by
scraping its stdout against a substring table copied from kairos-agent's own
`internal/agent/TUIconstants.go`: partition → before-install → active system →
GRUB → recovery → passive → after-install → finish.

Renders the wordmark, an ASCII spinner, `[####....] NN%`, and the footer
`press 'l' to show logs · do not power off`. On failure: red `install halted`, a
scrollable log viewport backed by a ring buffer, and `s` for a rescue shell via
`syscall.Exec`.

Palette is VGA-16-constrained exactly like the splash — lipgloss `"12"`, `"14"`,
`"15"`, with `"9"` red reserved for a genuine halt. Truecolor Tokyo Night hex
renders as garbage on the kernel VT.

### Wiring

`90_desktop_installer.yaml`'s drop-in points `ExecStart` at
`/usr/bin/hadron-install-ui`; its guard is unchanged.

The agent appliance's `20-agent.conf` still sorts after `10-desktop.conf` and
still wins, so `hadron-agent-install` keeps owning the appliance path. It gains
the progress screen by changing its `INSTALL_CMD` **default** from
`kairos-agent install` to `hadron-install-ui --progress-only`. Because this is
the default and not the variable itself, `test/agent/install_script_test.sh` and
its siblings keep injecting fakes through `HADRON_INSTALL_CMD` unchanged, and
the audited safety contract is untouched.

### Testing

- Go unit tests for the three pure pieces: step parser, cloud-config renderer,
  disk lister.
- The non-interactive environment path kept working, so existing integration
  tests port over.
- Build-time smoke test asserting the binary runs and degrades sanely without a
  tty.
- Shell assertions in the image test harness: binary present and executable,
  dracut module files present, `getty`/`ly` ordering symlinks intact.

## Risks

| Risk | Mitigation |
|---|---|
| buildx caches the kairos-init/dracut stage, shipping a stale initramfs splash | `--no-cache-filter` on the splash and kairos-init stages; assert splash presence inside the built initramfs in tests |
| kairos-init clobbers branding files | Re-`COPY` after kairos-init — pattern already used for `grubmenu.cfg` |
| AuroraBoot version differences in overlay handling (`v0.21.0-alpha.4` here vs `latest` in AIOS) | Verify the "only writes grub.cfg if absent" behaviour against the pinned version before relying on it |
| Rewriting the installer regresses an unattended/automation path | Preserve every environment seam; keep the non-interactive path covered by tests |
| bubbletea misbehaving on `TERM=linux` | AIOS runs the same stack on the same kernel VT; VGA-16 palette discipline is the known requirement |

## References

AIOS implementation, for the patterns ported here:

- `image/aios-splash/{main.c,Makefile}` — splash source and build flags
- `image/overlay/usr/lib/dracut/modules.d/50aios-splash/` — dracut module
- `image/overlay/etc/dracut.conf.d/50-aios-splash.conf`
- `auroraboot/{live-grub.cfg.tmpl,build.sh}` — live ISO branding
- `image/aios-install-ui/` — installer TUI (`steps.go` for the scrape table)
- `image/overlay/system/oem/90_aios_installer.yaml` — the drop-in wiring
- `docs/superpowers/specs/2026-06-01-aios-branding-design.md` — splash/getty
  ordering rationale
- `docs/superpowers/specs/2026-06-03-aios-install-ui-design.md` — upstream-verified
  analysis of `52_installer.yaml` and the step table
