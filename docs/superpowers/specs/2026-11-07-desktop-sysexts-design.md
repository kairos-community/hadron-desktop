# One core image, desktops as system extensions

## Intent

Today every release builds eight desktop images (four desktops, each in a
full and a vm GPU line) and nine ISOs. They are almost the same. 17,122 files,
822 MiB, are identical in all four vm images. Each desktop adds only 26 MiB
(i3) to 952 MiB (COSMIC).

This design ships one **core** image per GPU line and each desktop as a
**system extension** (sysext) built against that core. The user picks a
desktop in the installer. A switch to another desktop downloads only that
desktop. It also prepares a usable Trusted Boot edition: only the core stays
in RAM, and the desktop merges from disk.

Goals:

- One installer ISO per GPU line, with the desktop picked at install time.
- A desktop switch that downloads the desktop and reboots. No full image
  pull, and no reinstall.
- Upgrades stay atomic: core and desktop change together, and a rollback
  restores both.
- The feature set of each desktop does not change (AGENTS.md).

Non-goals: several desktops merged at the same time; a user choosing a
desktop at the login screen; desktops from outside this repository.

Anti-goals: a core and a desktop from different releases merged together; a
machine that boots to no login because no desktop merged.

## Measurements (2026-11-07, `*-vm-v0.0.8`, uncompressed)

These are from streaming the four published images and hashing every file.
The data and scripts are in the session scratchpad (`split/`,
`scripts/report.txt`).

| | Files | Size |
|---|---|---|
| Identical in all four (core) | 17,122 | 822.7 MiB |
| Shared by 2–3 desktops (ly, GTK3 and Adwaita cursors, X11 and Wayland libraries, Xwayland, cliphist) | | ~61 MiB |
| sway only, `/usr` | 2,456 | 72.6 MiB |
| i3 only, `/usr` | 2,870 | 58.0 MiB |
| niri + Noctalia only, `/usr` | 3,360 | 204.6 MiB |
| COSMIC only, `/usr` | 3,523 | 973.4 MiB |

- Desktop files under `/etc`: 40 for sway, 45 for i3, 32 for niri, 13 for
  COSMIC. Nothing lands in `/var`, `/opt` or `/root`.
- One release today: 4,896 MiB of vm images (2,254 MiB compressed). Split:
  about 938 MiB core plus sysexts of 29, 26, 147 and 952 MiB, so about
  2,090 MiB (−57%).
- One ISO with all four desktops bundled: about 1.0–1.1 GiB. v0.0.7's four
  ISOs total 2.3 GiB.
- Units each desktop enables: sway, i3 and niri enable `ly@tty1`; COSMIC
  enables `cosmic-greeter`, `cosmic-greeter-daemon` and `accounts-daemon`. The
  other 94 links are the same in all four.

Files the desktops change in shared paths (they have the same path but
different content):

- `/boot/initrd` (55 MiB): dracut bakes the desktop's splash branding into it.
- `/etc/passwd`, `group`, `shadow` and `gshadow`: COSMIC adds
  `cosmic-greeter`.
- `/etc/hadron-desktop/release` and `session`, `/etc/issue`, `/etc/motd`, and
  the GRUB `theme.txt`: the desktop name.
- i3's X11 build of Mesa and libepoxy (`libgallium`, `libEGL`, `libepoxy`,
  `xkbcli`), 19 MiB.
- `gschemas.compiled`: COSMIC compiles its own.
- `shortcuts.txt`, `/system/oem/91_desktop_session.yaml`.
- Some static `.a` files differ only because those builds are not
  reproducible.

The Trusted Boot prototype measured the RAM side (see
`2026-11-07-trusted-boot-edition-design.md`). sway's 974 MiB rootfs costs
977 MiB of RAM after boot with kairos-io/kairos#6023, or 1,919 MiB without it.
COSMIC's 1.9 GiB rootfs needs 8 GB machines. With this split, the RAM cost is
the core's alone, for every desktop.

## Design

### The pieces

- **Core image**, per GPU line, tagged `core-<tag>` and `core-vm-<tag>`. It
  holds the Hadron base and everything two or more desktops use: Mesa,
  PipeWire, NetworkManager, fonts, flatpak, gnome-keyring, polkit, the
  portals, GTK3 and the cursors, X11 and Wayland client libraries, Xwayland,
  cliphist, ly and greetd, the `hadron-*` scripts, hadron-update, the
  installer and the boot splash. The 61 MiB shared by 2–3 desktops goes in the
  core: stacking shared sysexts costs more than it saves.
- **Desktop sysexts**, one per desktop and core line, published as OCI
  artifacts `sysext/<desktop>:<tag>` and `<tag>-vm` with digests. Each holds
  its compositor or window manager and its own apps and configs, all under
  `/usr`.
- **Our catalog** (from the extensions design, phase 2) lists them, so
  `kairos-agent sysext install` resolves a desktop like any other extension.

### A desktop matches its core exactly

Each desktop sysext's `extension-release` file carries:

```
ID=hadron-desktop
VERSION_ID=<release tag>
HADRON_DESKTOP_LINE=full|vm
```

The core's `os-release` carries the same `ID` and `VERSION_ID`, so systemd
refuses to merge a desktop from another release or line. The release pipeline
builds the core and its desktops in one run, against one toolchain, so they
share every library.

The extensions conflict check (#58) runs on each desktop in CI. A desktop
sysext may add files but never replace a core file. A replaced file fails the
release.

### What has to move out of `/etc`

A sysext covers `/usr` only. Each desktop's `/etc` files move as follows:

| Today | After |
|---|---|
| Compositor and WM configs (`/etc/sway`, `/etc/i3`, `/etc/niri`), mako, dunst, foot, waybar, swaylock | `/usr/share/<tool>/` defaults, or a path passed with `-c`/`--config` from the session script. Most of these tools read an XDG dir in `/usr/share` or `/etc/xdg`, and `XDG_CONFIG_DIRS` can list `/usr/share/hadron/<desktop>/xdg` first |
| `/etc/<desktop>-desktop/env`, `/etc/xdg/autostart/*` | `/usr/share/hadron/<desktop>/`, read by the session script; autostart under `/usr/share/hadron/<desktop>/xdg/autostart` through `XDG_CONFIG_DIRS` |
| PAM files (`pam.d/ly`, `swaylock`, `i3lock`, greetd's) | the core, for every desktop. They are small and harmless when unused |
| ly and greetd configs | the core |
| Unit links in `/etc/systemd/system/*.wants` | `/usr/lib/systemd/system/*.wants` in the sysext, with a `systemctl daemon-reload` after the merge (systemd-sysext does this) |
| `cosmic-greeter` user | a `sysusers.d` file in the COSMIC sysext, applied by `systemd-sysusers` after the merge |
| `/etc/hadron-desktop/session`, `issue`, `motd`, GRUB `theme.txt` | written at boot by a core unit from the merged desktop's `/usr/share/hadron/<desktop>/session` |

If a file truly must sit in `/etc`, a **confext** (configuration extension,
`/etc` only) from the same build is the fallback. This design avoids confexts
for now: Kairos's sysext tooling does not manage them yet, and each one is a
second artifact to keep matched.

### The two shared-file conflicts

- **Mesa for i3.** i3's image builds Mesa and libepoxy with X11 support, and
  the other desktops build them for Wayland only. The core builds Mesa once,
  with both platforms (`-Dplatforms=wayland,x11`, GLX through DRI). That adds
  the X11 client libraries to the core, which are already shared by i3, niri
  and COSMIC through Xwayland. The i3 sysext then ships no Mesa.
- **GSettings schemas.** One `gschemas.compiled` cannot belong to two layers.
  The core ships the schema XML of every desktop, or only its own. A core unit
  ordered after `systemd-sysext.service` compiles them into
  `/run/hadron/schemas`, and `GSETTINGS_SCHEMA_DIR` points there. COSMIC's
  sysext ships its XML and no compiled file.

### Login when no desktop merged

The core masks `getty@tty1` today because each desktop owns the login screen.
After the split:

- The core installs both ly and greetd and enables neither.
- Each desktop sysext enables its login manager with a `.wants` link: ly for
  sway, i3 and niri; greetd for COSMIC.
- A core unit, `hadron-login-fallback.service`, starts `getty@tty1` when no
  desktop sysext merged. The console shows the reason and the command that
  fixes it (`hadron-ext install <desktop>`). A broken or missing desktop must
  never leave a machine with no way to log in.

### Branding in the initramfs

The initramfs is part of the core, so it cannot name a desktop. The splash and
GRUB theme show "Hadron Desktop" and the release. The desktop name moves to
the login screen and `/etc/issue`, which are written after the merge.

### GPU lines

Keep two core lines, full and vm, as today. Firmware and the Intel and AMD
drivers must be present when the initramfs loads the GPU modules, and a sysext
merges later. Turning the full line into a "hardware" sysext on one core is a
separate question for later.

### Install

The installer gets a **Pick your desktop** step, with COSMIC as the default.
It writes the choice as `install.extensions` in the cloud-config it already
renders. Kairos installs these into the boot slots at install time (read in
`sdk/types/install/install.go`).

The ISO bundles all four sysexts of its line under the live medium, so an
install needs no network. The installer references them by absolute path, for
example `/run/initramfs/live/extensions/sway.sysext.raw`.

### Upgrade and rollback

Desktops are installed per boot slot (`--active` and `--passive`), not
`--common`, so each slot holds the desktop that matches its core.
`hadron-update upgrade`:

1. resolves the new core and the matching desktop sysext from our catalog
   (same tag and line);
2. runs `kairos-agent upgrade` for the core into the passive slot;
3. installs the matching desktop into the passive slot;
4. on reboot, the new slot merges a matching pair. A rollback boots the old
   slot, which still has its old pair.

User extensions from the extensions feature stay in `--common`, as designed
there.

### Desktop switch

`hadron-update switch <desktop>` (and the tray's "Switch desktop") downloads
the other desktop sysext of the running release and line. It installs it into
the active slot in place of the current desktop and asks for a reboot. Nothing
else is downloaded. The previous desktop stays in the passive slot until the
next upgrade, so a rollback still works.

### Migration of installed systems

A machine on a full image today (`/etc/hadron-desktop/release` names its
flavor) upgrades to the first split release through a one-time path in
hadron-update. It installs the core image, then the desktop sysext that
matches the old flavor, into the passive slot. The previous full image stays
the fallback boot entry.

### Release pipeline

| Today | Split |
|---|---|
| 8 desktop images + agent | 2 cores + 8 desktop sysexts + agent |
| 9 ISOs | 2 ISOs (each with its line's 4 desktops) + agent |
| 4 COSMIC prebuilds, 8 ISO jobs | the same COSMIC prebuilds, 2 core jobs, 8 small sysext jobs |

A sysext job builds the desktop's stage on top of the core, takes the
difference (only added files under `/usr`), and checks it with the conflict
check. Then it packs it with `auroraboot sysext` and signs it: cosign for the
OCI artifact, and the Trusted Boot keys on that line.

The agent image stays a full i3-based image for now. It is an appliance with
its own gates, and it gains nothing from the split.

### Trusted Boot

The Trusted Boot edition UKI contains only the core, so the RAM cost is the
core's for every desktop (about 940 MiB, or less with #6023). The desktop
sysext merges from the encrypted disk, signed with the edition's keys.
COSMIC becomes possible on Trusted Boot machines with less than 8 GB.

## Phases

1. **Prepare in the current layout.** These changes are useful on their own:
   - move the desktop configs out of `/etc`;
   - unit links to `/usr/lib`;
   - the `sysusers.d` user;
   - one Mesa build with X11;
   - schemas compiled at boot;
   - a desktop-neutral initramfs.

   After this, a desktop image differs from the core only by added `/usr`
   files. A CI check proves it by comparing the four images, like the
   measurement above.
2. **Build and publish** the core images and desktop sysexts next to today's
   images, with no change for users. Test install, switch, upgrade and
   rollback in QEMU.
3. **The installer and hadron-update** use the split: the desktop step,
   per-slot desktops, switch, and migration. The old per-desktop images stop
   with that release.
4. **The Trusted Boot edition** on the core.

## Risks

- **No login after a failed merge.** The fallback getty covers this. It needs
  a test that boots a core with no desktop.
- **Kairos per-slot extensions under upgrade.** `kairos-agent upgrade` must
  keep the passive slot's extensions in step with the new core. This needs a
  check in the Kairos code, and maybe a Kairos change.
- **COSMIC is half the release.** The split does not make COSMIC smaller. It
  only stops other desktops from carrying it.
- **Two GPU lines double the sysexts.** A later "hardware" sysext could fold
  them, but early firmware loading makes that hard.

## Left to the builder

- Exact `/usr/share` paths for each tool's default config.
- Whether the getty fallback is a unit condition or a generator.
- The catalog layout for desktop sysexts next to user extensions. A separate
  catalog file is acceptable.
