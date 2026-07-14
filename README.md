# Hadron desktop

Two tiling desktop variants plus an opt-in agent overlay built on top of the
minimal [Hadron](https://github.com/kairos-io/hadron) base image:

| `DESKTOP` | Display stack | Desktop | Native desktop tools |
|-----------|---------------|---------|----------------------|
| `sway` (default) | Wayland + wlroots | [Sway](https://swaywm.org/) | foot, fuzzel, waybar, mako |
| `i3` | [XLibre](https://www.xlibre.net/) X server | [i3](https://i3wm.org/) | st, dmenu, i3bar, dunst |

Both include NetworkManager, PipeWire audio, wifi, bluetooth, Docker and
Distrobox. Everything is compiled from source against the Hadron musl toolchain
in one multi-stage `Dockerfile`; BuildKit only evaluates the selected graphics
stack.

The repo is self-contained (`Dockerfile`, common and variant rootfs overlays,
and a `test/` harness) and depends only on the published Hadron images
(`ghcr.io/kairos-io/hadron{,-toolchain}:main`) — no Hadron source checkout needed.

> Status: built incrementally — see the milestone table below.

## Variant architecture

- **Sway:** Wayland, wlroots, Wayland Mesa, Sway, foot, fuzzel, waybar, mako,
  wl-clipboard, slurp and swayidle.
- **i3:** XLibre 25.2.0, X11 Mesa/GLX, the XLibre libinput driver, i3 4.25.1,
  st, dmenu, i3bar, dunst and xsel.
- **Agent overlay:** an opt-in i3-based appliance build that layers Cua and
  the agent services on top of the XLibre desktop.
- **Login:** `ly` runs on `tty1`. It launches Sway directly for the Wayland
  session and owns the XLibre server lifecycle for the i3 X session.
- **Networking:** NetworkManager + wpa_supplicant (wifi).
- **Audio:** PipeWire + WirePlumber.
- **Bluetooth:** BlueZ.
- **Containers:** rootful docker + distrobox (mutable dev-env containers).
- **Real-hardware firmware:** optional curated `linux-firmware` subset
  (`--build-arg FIRMWARE=true`).
- **Hardware GL:** optional Mesa `iris`/`radeonsi` (`--build-arg GPU=full`).

## Build

```sh
make image                 # default Wayland/Sway image: sway-desktop:dev
make DESKTOP=i3 image      # XLibre/i3 image: i3-desktop:dev
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

Only `sway` and `i3` are valid selector values. The corresponding artifacts are
kept under `build/sway-desktop/` and `build/i3-desktop/`.

## Releases

Pushing a `v`-prefixed tag runs `.github/workflows/release.yml`. The workflow
builds the Sway and i3/XLibre installer ISOs in parallel, builds the separate
agent ISO, verifies all SHA-256 checksums, and creates a GitHub Release
containing three ISOs and three checksum files:

```sh
git tag -a v1.0.0 -m "Hadron Desktop v1.0.0"
git push origin v1.0.0
```

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

### Users and login

The image bakes in **no user**. The desktop user is created at install time and
lives on the persistent `/home`. On boot the **`ly`** display manager (TUI, on
tty1) authenticates that user and launches the selected session through
`/usr/bin/start-desktop`. In the Sway image this is the Wayland session; in the
i3 image, `ly` starts XLibre and then launches i3.

There are two ways to create that user:

- **Interactive installer (default).** Boot the live ISO with nothing else and a
  small wizard (`/usr/local/bin/hadron-install`, wired in via
  `system/oem/90_desktop_installer.yaml`) prompts on tty1 for **hostname, username,
  password, and target disk**, assigns the desktop groups (admin, audio, video,
  render, input, bluetooth, seat), hashes the password (`openssl passwd -6`),
  writes the cloud-config, and installs.
- **Unattended cloud-config.** Provide a `cloud-config.yaml` (`users:`/`install:`
  block — see the example file) via AuroraBoot `--cloud-config` or a datasource.
  When one is present the wizard detects it and runs the normal unattended
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

This bundles a curated `linux-firmware` subset (iwlwifi, ath, rtw, brcm, intel
bluetooth, i915, amdgpu, …) into `/lib/firmware`. Wifi/BT/audio on real hardware
is validated by booting on a physical machine.

### Hardware GPU (accelerated GL)

The default Mesa build (`GPU=vm`) ships only software/virtual drivers
(virgl/softpipe/svga) — correct for QEMU and needs nothing from LLVM. For
accelerated GL on real Intel/AMD laptops, build with `GPU=full`:

```sh
docker build --build-arg GPU=full --build-arg FIRMWARE=true \
  -t sway-desktop:hw .

docker build --build-arg DESKTOP=i3 --build-arg GPU=full \
  --build-arg FIRMWARE=true -t i3-desktop:hw .
```

`GPU=full` builds Mesa `iris` (Intel) + `radeonsi` (AMD), which require LLVM.
The example builds its **own** LLVM/clang/libclc + SPIRV stack on top of the
published `hadron-toolchain` (same musl ABI — no Alpine cross-mix) as dedicated
stages in this Dockerfile, so **the main Hadron toolchain is never touched and
no extra orchestration is needed** — a plain `docker build` is enough. Those
stages are gated: for `GPU=vm` (the default) BuildKit prunes them, so a normal
build never compiles LLVM.

For the hardware path the example flips `-Dllvm=true`, sets `-Dcpp_rtti=false`
(its libLLVM is built without RTTI), and bundles `libLLVM.so` + `libelf.so` into
the image (the megadriver links them at runtime, ~125 MB). Building LLVM adds
~15–20 min to the `GPU=full` build.

Validated: Mesa 25.3 builds `iris`/`radeonsi`/`virgl`/`softpipe` into
`libgallium-25.3.0.so` against the example-built libLLVM, the megadriver resolves
all runtime symbols, and the resulting ISO boots. Actual GPU *rendering* is
validated on physical hardware — QEMU has no real GPU, so a VM boot falls back to
softpipe/virgl (software) while the hardware drivers ride along for real metal.

## Layout

```
hadron-desktop/
  Dockerfile          # multi-stage build of the whole desktop stack
  cloud-config.yaml   # example Kairos install config (creates the desktop user)
  rootfs/             # shared OS, installer and service overlay
  rootfs-sway/        # Wayland/Sway session config and native tools
  rootfs-i3/          # XLibre/i3 session config and native tools
  tools/
    hadron-xroot.c    # tiny X11 root-window background helper
    vm.sh             # variant-aware QEMU launcher
  test/
    run.sh            # default Sway build -> ISO -> QEMU -> assert
    Dockerfile.test   # injects in-guest test instrumentation
    guest/check.sh    # in-guest assertions (emit SWAYTEST: markers)
    artifacts/        # console logs + screenshots (gitignored)
```
