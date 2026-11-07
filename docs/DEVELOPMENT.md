# Developing Hadron Desktop

Four desktop variants plus an opt-in agent overlay built on top of the
minimal [Hadron](https://github.com/kairos-io/hadron) base image. COSMIC is the
one for people new to Linux; niri with Noctalia is a polished scrolling-tiling
desktop; Sway and i3 are tiling desktops for people who already know them.

| `DESKTOP` | Display stack | Desktop | Native desktop tools |
|-----------|---------------|---------|----------------------|
| `sway` (default) | Wayland + wlroots | [Sway](https://swaywm.org/) | foot, fuzzel, waybar, mako |
| `i3` | [XLibre](https://www.xlibre.net/) X server | [i3](https://i3wm.org/) | st, dmenu, i3bar, dunst |
| `cosmic` | Wayland + Smithay | [COSMIC](https://system76.com/cosmic) Epoch 1 | Files, Terminal, Text Editor, Settings, Store (Flathub), graphical login, first-run setup |
| `niri` | Wayland + Smithay | [niri](https://github.com/YaLTeR/niri) with the [Noctalia](https://github.com/noctalia-dev/noctalia) shell | foot; Noctalia's bar, dock, launcher, control center, notifications, lock screen |

All of them include NetworkManager, PipeWire audio, wifi, bluetooth, Docker and
Distrobox. Everything is compiled from source against the Hadron musl toolchain
in one multi-stage `Dockerfile`; BuildKit only evaluates the selected graphics
stack.

The repo is self-contained (`Dockerfile`, common and variant rootfs overlays,
and a `test/` harness) and depends only on published images: a pinned Hadron
release (`ghcr.io/kairos-io/hadron{,-toolchain}:<HADRON_VERSION>`) and
kairos-init from the Kairos monorepo (`ghcr.io/kairos-io/kairos/kairos-init:master`)
— no Hadron or Kairos source checkout needed.

> Status: built incrementally — see the milestone table below.

## Variant architecture

- **Sway:** Wayland, wlroots, Wayland Mesa, Sway, foot, fuzzel, waybar, mako,
  wl-clipboard, slurp, swayidle and swaylock.
- **i3:** XLibre 25.2.0, X11 Mesa/GLX, the XLibre libinput driver, i3 4.25.1,
  st, dmenu, i3bar, dunst, xsel, i3lock and xss-lock, and shotgun, hacksaw and
  xclip for screenshots.
- **COSMIC:** COSMIC Epoch 1.11 (the cosmic-comp compositor, panel and dock,
  launcher, Settings, Files, Terminal, Text Editor, Store), built from source
  in Rust against the Hadron musl libraries. polkit (duktape) and
  accountsservice back the Settings panels that need privileges; the
  xdg-desktop-portal stack backs Flatpak file dialogs and screen sharing;
  Xwayland runs X11 apps.
- **niri:** niri 26.04 (a scrollable-tiling compositor: windows open in
  columns on an endless strip, so a new window never squeezes the others) and
  Noctalia 5 (C++, no Qt or GTK), both built from source against the Hadron
  musl libraries. Noctalia is the polkit agent and the notification server;
  xwayland-satellite runs X11 apps; xdg-desktop-portal-gtk backs Flatpak file
  dialogs. The defaults are in `/etc/niri/config.kdl`; copy it to
  `~/.config/niri/config.kdl` to change it.
- **Agent overlay:** an opt-in i3-based appliance build that layers Cua and
  the agent services on top of the XLibre desktop.
- **Login:** on Sway, i3 and niri, `ly` runs on `tty1`. It launches Sway directly for
  the Wayland session and owns the XLibre server lifecycle for the i3 X session.
  On COSMIC, greetd runs the graphical cosmic-greeter on `tty1`, and the first
  login opens COSMIC's welcome (cosmic-initial-setup).
- **Networking:** NetworkManager + wpa_supplicant (wifi).
- **Audio:** PipeWire + WirePlumber.
- **Bluetooth:** BlueZ. On Sway and i3, Super+Shift+B opens a menu to turn
  the adapter on or off and to pair, connect, disconnect or forget a device;
  niri and COSMIC use the Bluetooth panel of their shell.
- **Hadron menu:** Super+Escape on every variant opens one menu for apps,
  Wi-Fi, Bluetooth, audio, screenshots, updates, the shortcut sheet and the
  power actions.
- **Night light:** on Sway, i3 and niri, Super+Alt+N (or "Night light" in
  the Hadron menu) turns on a warmer screen from 20:00 to 07:00, and the
  setting is kept across logins. COSMIC has no night light yet.
- **Hardware keys:** volume, mute, screen brightness and play/pause/next/previous
  work on every variant and show the new level on screen.
- **Containers:** rootful docker + distrobox (mutable dev-env containers).
- **Flatpak portals:** xdg-desktop-portal on every variant, so Flatpak apps
  get file dialogs. Sway uses xdg-desktop-portal-gtk, plus
  xdg-desktop-portal-wlr for screenshots and screen sharing; i3 uses
  xdg-desktop-portal-gtk on a GTK3 built for X11; niri uses
  xdg-desktop-portal-gtk; COSMIC uses xdg-desktop-portal-cosmic.
- **Dark mode:** Flatpak apps follow the dark colour scheme through the
  portal's Settings interface. On Sway, i3 and niri the GTK portal reads it
  from a GSettings vendor override (`color-scheme='prefer-dark'`, GTK3 theme
  `Adwaita-dark`); on COSMIC the COSMIC portal reports COSMIC's own theme.
- **Two images per desktop:** the standard one, for desktops and laptops, has
  Mesa's Intel (`iris`) and AMD (`radeonsi`) drivers and a curated
  `linux-firmware` subset (`GPU=full FIRMWARE=true`). The VM one has only
  `llvmpipe`, `virgl` and the other virtual drivers, and no firmware
  (`GPU=vm`, the default of a local build). Both run in a VM; the VM one is
  smaller. See [Releases](#releases).

## Build

```sh
make image                 # default Wayland/Sway image: sway-desktop:dev
make DESKTOP=i3 image      # XLibre/i3 image: i3-desktop:dev
make DESKTOP=cosmic image  # COSMIC image: cosmic-desktop:dev
make DESKTOP=niri image    # niri + Noctalia image: niri-desktop:dev
make images                # build both image variants
make agent-image           # build the opt-in agent overlay image
make agent-iso             # build the opt-in agent ISO

make                       # default Sway image + installer ISO
make DESKTOP=i3 iso        # XLibre/i3 image + installer ISO
make agent-iso VERSION=v1.2.3
```

The equivalent direct Docker builds are:

```sh
docker build -t sway-desktop:dev .
docker build --build-arg DESKTOP=i3 -t i3-desktop:dev .
```

Only `sway`, `i3`, `cosmic` and `niri` are valid selector values. The corresponding artifacts are
kept under `build/sway-desktop/` and `build/i3-desktop/`.

## Everyday use

- **Shortcuts:** `Super+F1` opens a cheat sheet. `Super+D` launches apps,
  `Super+Enter` opens a terminal, `Super+Shift+E` opens the power menu (log
  out, suspend, reboot, power off). A one-time notification on the first login
  points at all three. In Sway, i3 and niri, `Super+Alt+L` locks the screen
  and `Print` takes a screenshot. Sway and i3 also lock the screen when idle
  and before suspend.
- **Apps:** Hadron includes no browser. At your first login, when the
  network is up, a short checklist offers a browser (Firefox, Chromium, Brave
  or LibreWolf) and some essential apps, and installs them from Flathub for
  your user. Skip it if you like; "Install apps" in the Hadron menu or
  `hadron-first-run-apps` opens it again. Anything else:
  `flatpak install flathub <app>`; `distrobox` for a mutable Linux to tinker
  in.
- **Updates:** a tray icon shows the installed release and tells you when a
  new Hadron Desktop release is out; its menu checks, installs, reboots and
  pins. Everything the icon does is also in the terminal:

  ```sh
  hadron-update                 # installed vs latest release (same as `status`)
  hadron-update upgrade         # install the latest; asks, then offers a reboot
  hadron-update upgrade --version v0.1.2   # a specific release (also rolls back)
  hadron-update switch <flavor> # move to another flavor's image line
  hadron-update switch --gpu vm # move to the VM image (--gpu full: back)
  hadron-update reboot          # reboot into the installed upgrade
  hadron-update rollback        # boot the previous system once (no download)
  ```

  Installing and rebooting go through a root service, `hadron-updated`, and
  polkit allows them for members of the `admin` group only; other users get a
  refusal, not a password prompt. The terminal and the tray use the same
  service, so a job started in one shows in the other.

  Upgrades are A/B: `kairos-agent upgrade` writes the new image next to the
  running one, which stays untouched until the reboot, and the previous system
  is kept as the fallback boot entry.

  Advanced mode is on by default: it lets you install any OCI image, not only
  a Hadron Desktop release (`hadron-update upgrade --image REF`, or "Install
  an image..." in the tray menu). To allow releases only, put `advanced=false`
  in `/etc/hadron-desktop/update.conf`. An image you install on purpose is
  pinned (`/var/lib/hadron-update/pin`): the tray shows it as pinned and stops
  offering "updates" over it until you pick the latest release again.
  Upgrade images are signed with cosign (keyless) and carry the labels
  `io.kairos.hadron-desktop.{flavor,version,hadron-version,gpu,firmware}`, which the
  updater uses to recognise a Hadron Desktop image. The agent flavor has no
  tray and no service; its CLI keeps the direct `sudo` path.

## Boot splash

`splash/` builds the boot animation and installs it as `/usr/bin/kairos-splash`.
kairos-init keeps a binary already at that path, and its dracut module and
`kairos-splash.service` then run it in the initramfs and in the booted system.
So there is one animation from firmware to login, with no splash units of our
own. It draws straight into the framebuffer (simpledrm, then whichever DRM
driver takes over) over the same gradient as the GRUB theme, and falls back to
a text animation on a console without one. Add `kairos.splash=0` at the boot
menu to turn it off for one boot. See [`splash/README.md`](../splash/README.md).

The boot menu also sets the Linux console's 16 colours to the Hadron palette
(`vt.default_red/grn/blu` on the kernel command line, generated from
`theme/palette.toml`), so the boot messages, the ly login and the installer
sit on the desktop's navy rather than black.

## Following Hadron

The Hadron release is pinned in one value kept in four places (the
`Makefile`, `Dockerfile`, `Dockerfile.agent` and the agent gate's
`test/agent/Dockerfile.compat`). `scripts/bump-hadron.sh vX.Y.Z` moves them all; `--check` fails if they ever disagree, and CI runs it.

`.github/workflows/follow-hadron.yml` runs daily. When kairos-io/hadron
publishes a newer release (and its images are pullable), it pins it, commits to
`main`, tags the next patch version and dispatches the release pipeline below.
A failed release publishes nothing, so installed systems never see it.

## Updating components

Every other source version is an `ARG <NAME>_VERSION=` in the Dockerfiles,
under a `# renovate:` comment that names where its releases are tagged.
Renovate (`renovate.json`) opens update pull requests on Monday mornings;
major updates wait for approval on its Dependency Dashboard issue. A new pin
needs the comment, or an entry in `scripts/check-renovate-pins.sh` with the
reason it is updated by hand. CI runs that script.

## Releases

Pushing a `v`-prefixed tag runs `.github/workflows/release.yml`. The workflow
builds the Sway, i3/XLibre, COSMIC and niri installer ISOs in parallel and
builds the agent ISO in a job that first runs all five agent gates. It then verifies every
asset and creates a GitHub Release:

```sh
git tag -a v1.0.0 -m "Hadron Desktop v1.0.0"
git push origin v1.0.0
```

Every desktop flavor is released in two lines:

| Line | Build | ISO and upgrade image | For |
| --- | --- | --- | --- |
| full | `GPU=full FIRMWARE=true` | `hadron-desktop-<flavor>-<tag>-amd64.iso`, `:<flavor>-<tag>` | desktops and laptops: Intel and AMD GPU drivers, Wi-Fi, Bluetooth and GPU firmware. Also runs in a VM (llvmpipe, virgl) |
| vm | `GPU=vm` | `hadron-desktop-<flavor>-vm-<tag>-amd64.iso`, `:<flavor>-vm-<tag>` | virtual machines: about 0.5 GB smaller, no hardware GPU drivers, no firmware |

The agent is a VM appliance and has one line, built with `GPU=vm`:
`hadron-desktop-agent-<tag>-amd64.iso` and `:agent-<tag>`.

Every release also pushes the upgrade images above to
`ghcr.io/kairos-community/hadron-desktop` (the agent image only after all five
gates pass). `hadron-update` installs those; it only considers tags GitHub
reports as the latest *published* release. An installed system stays on its
line: `/etc/hadron-desktop/release` records `GPU=full` or `GPU=vm`, and upgrades
pull the image of that line. Systems installed from v0.0.7 or earlier have no
`GPU` key and follow the full line (their own updater already pulls
`<flavor>-<tag>`). `hadron-update switch --gpu vm|full` or the tray menu moves a
system to the other line. Each pushed image is signed with cosign (keyless, by
digest) and labelled with its flavor, line and release tag;
`scripts/release-manifest.sh` fails the release if they disagree.

Each release carries thirty-six files — nine ISOs (four flavors in two lines,
and the agent), and for every ISO a checksum, an SPDX SBOM, and a build
manifest:

| Asset | Contents |
| --- | --- |
| `hadron-desktop-<flavor>[-vm]-<tag>-amd64.iso` | the bootable installer |
| `….iso.sha256` | its SHA-256, in `sha256sum --check` format |
| `….iso.spdx.json` | SPDX JSON SBOM of the OCI image the ISO wraps, produced by `anchore/syft:v1.29.0` |
| `….iso.build.json` | the build manifest described below |

`<flavor>` is `sway`, `i3`, `cosmic`, `niri`, or `agent`. The build manifest records exactly what
went into the image, so a downloaded ISO can be traced back to its sources:

```json
{
  "release_tag": "v1.0.0",
  "git_commit": "…",
  "hadron_base_reference": "ghcr.io/kairos-io/hadron:v0.6.3",
  "hadron_base_digest": "sha256:…",
  "image_id": "sha256:…",
  "iso": "hadron-desktop-agent-v1.0.0-amd64.iso",
  "iso_sha256": "…",
  "desktop_flavor": "agent",
  "gpu": "vm",
  "firmware": false,
  "xlibre_version": "25.2.0",
  "cua_revision": "…",
  "atspi_version": "2.54.0"
}
```

All thirteen keys are present in every manifest. `gpu` is the line (`full` or
`vm`, always `vm` for agent), and `firmware` is `true` exactly on the full line.
`xlibre_version` is set only for i3
and agent, the two flavors that run the XLibre X server, and
`cua_revision`/`atspi_version` are `null` for every flavor except agent.

The publishing job refuses to upload unless it finds exactly nine ISOs, nine
checksums, nine SBOMs, and nine manifests, one for each flavor and line, every
checksum verifies, every SBOM
parses as SPDX with a non-empty package list, and every manifest carries valid
values that match the ISO shipped beside it. The agent job uploads nothing at
all — not even its ISO — until the compatibility, contract, install, recovery,
and UI gates have all passed. Release artifacts are not signed.

An existing tag can be rebuilt from the Actions page with the workflow's manual
dispatch. Rebuilt assets replace same-named assets on a mutable release.

## Try it in a VM

```sh
make vm-install   # fresh disk, boot the newest installer ISO (then it reboots to disk)
make vm           # boot the already-installed disk

DESKTOP=i3 make vm-install
DESKTOP=i3 make vm
```

Both call `tools/vm.sh`, which launches QEMU with UEFI (OVMF) **and virtio-gpu**.
Use it rather than a hand-rolled `qemu-system-x86_64` line: QEMU's *default* VGA
gives a glitchy UEFI framebuffer that mangles the boot console into colored
static (a QEMU artifact, not an image bug — real hardware renders fine).
Connect over VNC (`<host>:5910`); set `NOVNC=1` for a browser client. See the
header of `tools/vm.sh` for knobs (`MEM`, `VNC`, `FRESH=1`, `ISO=…`, …).

The Kairos init layer is folded into the Dockerfile's final stage, so either
Docker command above already produces a bootable artifact
AuroraBoot can turn into an ISO (build `--target default` for the bare desktop
image without it).

## Run on real hardware

The image is a bootable Kairos/Hadron OS image. Build an ISO/disk with AuroraBoot
(`make iso`) and install it to a machine. On boot, `ly` runs on `tty1` and starts
the session selected at image build time. (Real wifi/bluetooth/audio require the
`linux-firmware` blobs — see milestone M6.)

## Test (Sway headless harness)

The existing `test/` harness exercises the default Sway variant, builds a bootable ISO
with AuroraBoot, boots it headless in QEMU, and asserts that the desktop and each
subsystem come up — entirely without a display. It exercises even wifi and
bluetooth using virtual kernel devices (`mac80211_hwsim`, `hci_vhci`).

```sh
# Run all assertions up to a given milestone (default M0)
MILESTONE=M1 test/run.sh

# Faster iteration
SKIP_BUILD=1 test/run.sh   # reuse built images
SKIP_ISO=1   test/run.sh   # reuse existing ISO
```

The guest emits `SWAYTEST: PASS/FAIL <name>` markers on the serial console; the
harness parses them and exits non-zero on any failure. Screenshots captured with
`grim` are written to a scratch disk and extracted to `test/artifacts/`.

The `test/agent/` directory holds the agent overlay's own test suite
(`run.sh compatibility` boots the real graphical XLibre/i3 session in QEMU and
drives it with `cua-driver`; focused scripts like `systemd_units_test.sh` and
`session_launcher_test.sh` check narrower slices statically). Notably,
`profile_isolation.sh` builds (or, with `SKIP_BUILD=1`, reuses) all three
images — `sway-desktop:dev`, `i3-desktop:dev`, `agent-desktop:dev` — exports
each one's real filesystem with `docker export`, and asserts against that
export: the plain Sway/i3 images contain none of the agent-only files,
units, or accounts, the agent image contains all of them, and the generic
agent image's file contents carry no live bearer, no configured digest, and
no PEM private key:

```sh
bash test/agent/profile_isolation.sh
SKIP_BUILD=1 bash test/agent/profile_isolation.sh   # reuse already-built images
```

### Users and login

The image bakes in **no user**. The desktop user is created at install time and
lives on the persistent `/home`. Kairos' default `kairos` user (password
`kairos`) exists only on the live ISO and in UKI install mode: the Dockerfile
`kairos` stage limits kairos-init's `10_accounting.yaml` to those boots, so an
installed system, and its login screen, shows only the users you created. An
upgraded system loses the old locked `kairos` user on its first boot, because
`/etc` is rebuilt at every boot; `/home/kairos`, if it exists, stays. On boot the **`ly`** display manager (TUI, on
tty1) authenticates that user and launches the selected session through
`/usr/bin/start-desktop`. In the Sway image this is the Wayland session; in the
i3 image, `ly` starts XLibre and then launches i3.

There are two ways to create that user:

- **Interactive installer (default).** Boot the live ISO with nothing else and
  `hadron-install-ui` — a Go TUI at `/usr/bin/hadron-install-ui`, wired in via
  `system/oem/90_desktop_installer.yaml` — runs on tty1. It collects **hostname,
  username, password, optional GitHub SSH keys and target disk** behind a review
  gate, assigns the desktop groups (admin, audio, video, render, input,
  bluetooth, seat), hashes the password (`openssl passwd -6`), writes the
  cloud-config, then runs `kairos-agent install` behind a branded progress
  screen (`l` toggles the raw logs, `s` opens a rescue shell if the install
  fails). Source lives in `install-ui/`.
- **Unattended cloud-config.** Provide a `cloud-config.yaml` (`users:`/`install:`
  block — see the example file) via AuroraBoot `--cloud-config` or a datasource.
  When one is present the TUI detects it and runs the normal unattended
  install instead of prompting — so CI and automated installs are unaffected.

The top-level build targets are:

```sh
make DESKTOP=sway iso
make DESKTOP=i3 iso
make agent-iso VERSION=v1.2.3
```

`agent-iso` first builds the i3 base image, then layers `Dockerfile.agent` on
top to produce the opt-in agent appliance. The agent ISO still uses the same
unattended install mechanism: if the datasource already contains `users:` and
`install:` the wizard skips the prompt path. For the agent appliance, the seed
also needs `hadron_agent.enabled=true` so the overlay starts its agent profile
instead of the plain desktop path.

Minimal unattended agent seed:

```yaml
#cloud-config
install:
  auto: true
  device: /dev/vda
  reboot: true
users:
  - name: agent
    passwd: "$6$..."
hadron_agent:
  enabled: true
  auth:
    user_token_hash: "sha256:..."
```

**Tokens and keys are runtime seed data, never Docker build args.** The bearer
digests (`user_token_hash`/`admin_token_hash`) and any TLS material for the
gateway are written into the datasource/cloud-config that AuroraBoot or the
installer feeds the *booted* image; they are read by the OEM provisioning
stage at boot (`system/oem/90_agent_profile.yaml`), not baked into a layer by
`agent-image`/`agent-iso`. Those two targets only ever take `BASE_IMAGE`,
`AGENT_BASE_IMAGE` and `VERSION` as `--build-arg`s (see the `Makefile`) — no
credential ever needs to (or should) flow through `docker build`. This is
also why the generic `agent-desktop`/agent ISO ships with no bearer, no
configured digest and no private key embedded in it:
`test/agent/profile_isolation.sh` exports the actual built images and proves
it (both that the plain Sway/i3 images carry none of the agent files, and
that the generic agent image's filesystem contents contain no live secret).

### Production vs test launch

`ly`'s interactive TUI login can't be driven over a headless VT, and headless
QEMU has no active VT / `virtio-gpu` gets zero scanouts (`-display none`). So the
**test image** (via `test/Dockerfile.test`) creates a throwaway user, **masks
`ly`**, and launches Sway from a dedicated `sway-headless.service` using the
wlroots **headless backend + pixman** renderer — the canonical way Sway is
exercised in CI. The harness still asserts `ly` is installed and wired up. The
real `ly` → DRM login path is validated on physical hardware.

## Milestones

All green and verified by the headless harness (M6's firmware blobs excepted —
those are hardware-validated):

| # | Scope | Autonomous test |
|---|-------|-----------------|
| M0 | e2e QEMU harness | boot, serial markers, scratch-disk round-trip |
| M1 | Sway + logind seat + foot | logind session, `swaymsg`, active output, grim screenshot |
| M2 | NetworkManager + wifi | DHCP on virtio-net + `mac80211_hwsim`/hostapd association |
| M3 | PipeWire + WirePlumber | user services up, `wpctl` sink from emulated HDA |
| M4 | BlueZ bluetooth | virtual adapter via `hci_vhci`+`btvirt`, `bluetoothctl`, bluez5 plugin |
| M5 | Desktop polish | swaybar, mako, fuzzel, wl-clipboard, slurp, swayidle |
| M6 | Real-hardware firmware | `regulatory.db` present; vendor blobs via `--build-arg FIRMWARE=true` (manual HW validation) |

### Real hardware

The default image is VM-slim. For real laptops, build with the firmware subset:

```sh
docker build --build-arg FIRMWARE=true -t sway-desktop:hw .
docker build --build-arg DESKTOP=i3 --build-arg FIRMWARE=true -t i3-desktop:hw .
```

This bundles a curated subset of a pinned `linux-firmware` release (Intel,
Qualcomm Atheros, Realtek, Broadcom, Marvell and MediaTek Wi-Fi and Bluetooth;
Intel, AMD and NVIDIA graphics) into `/usr/lib/firmware`, compressed with zstd:
about 1 GB of blobs as about 500 MB. They must not go to
`/usr/local/lib/firmware`, which an installed system mounts its persistent
partition over. Wi-Fi, Bluetooth and GPU firmware loading is checked by booting
on a physical machine; CI does not test it.

### Hardware GPU (accelerated GL)

The default Mesa build (`GPU=vm`) ships only software and virtual drivers:
`llvmpipe`, `virgl`, `softpipe` and `svga`. In a VM with 3D acceleration
(virtio-gpu with OpenGL) Mesa uses `virgl` and renders on the host GPU. On any
other display (virtio-gpu or QXL without 3D, `-vga std`, or real hardware with
`GPU=vm`) it uses `llvmpipe`, a multi-threaded rasterizer that compiles shaders
with LLVM. `softpipe` is only a fallback that `GALLIUM_DRIVER=softpipe`
selects: it is 60-190 times slower than `llvmpipe` (a key press in the COSMIC
greeter took about 6 s to reach the screen). For accelerated GL on real
Intel/AMD laptops, build with `GPU=full`, as the release builds the full line:

```sh
docker build --build-arg GPU=full --build-arg FIRMWARE=true \
  -t sway-desktop:hw .

docker build --build-arg DESKTOP=i3 --build-arg GPU=full \
  --build-arg FIRMWARE=true -t i3-desktop:hw .
```

`GPU=full` also builds Mesa `iris` (Intel) + `radeonsi` (AMD). Every mode links
LLVM (`llvmpipe` needs it); `iris` and `radeonsi` also need libclc and SPIRV.
The example builds its **own** LLVM/clang/libclc + SPIRV stack on top of the
published `hadron-toolchain` (same musl ABI — no Alpine cross-mix) as dedicated
stages in this Dockerfile, so **the main Hadron toolchain is never touched and
no extra orchestration is needed** — a plain `docker build` is enough. The
SPIRV stages are gated: for `GPU=vm` (the default) BuildKit prunes them. LLVM
itself compiles for about three hours; CI and release builds take it from a
prebuilt image instead (`LLVM_IMAGE`, see the Makefile).

Mesa is built with `-Dllvm=enabled` and `-Dcpp_rtti=false` (the libLLVM is built
without RTTI). The image bundles only `libLLVM.so` (~125 MB), and for
`GPU=full` also `libelf.so`, because the megadriver links them at runtime.

Validated: Mesa 25.3 builds `iris`/`radeonsi`/`virgl`/`softpipe` into
`libgallium-25.3.0.so` against the example-built libLLVM, the megadriver resolves
all runtime symbols, and the resulting ISO boots. Actual GPU *rendering* is
validated on physical hardware — QEMU has no real GPU, so a VM boot uses
llvmpipe or virgl while the hardware drivers ride along for real metal.

## Layout

```
hadron-desktop/
  Dockerfile          # multi-stage build of the whole desktop stack
  cloud-config.yaml   # example Kairos install config (creates the desktop user)
  rootfs/             # shared OS, installer and service overlay
  install-ui/         # Go TUI installer (-> /usr/bin/hadron-install-ui)
  update/             # update checker / notifier / upgrade CLI (-> /usr/bin/hadron-update)
  scripts/            # release manifest, Hadron pin bump
  rootfs-sway/        # Wayland/Sway session config and native tools
  rootfs-i3/          # XLibre/i3 session config and native tools
  tools/
    hadron-xroot.c    # i3 wallpaper: a PNG scaled to fill each monitor, or a colour
    gen-wallpaper.py  # draws the Hadron wallpapers from theme/palette.toml (at build time)
    vm.sh             # variant-aware QEMU launcher
  test/
    run.sh            # default Sway build -> ISO -> QEMU -> assert
    Dockerfile.test   # injects in-guest test instrumentation
    guest/check.sh    # in-guest assertions (emit SWAYTEST: markers)
    artifacts/        # console logs + screenshots (gitignored)
```
