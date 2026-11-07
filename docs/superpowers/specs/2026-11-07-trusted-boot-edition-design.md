# Trusted Boot edition (UKI, Secure Boot, TPM-sealed encryption)

Date: 2026-11-07. Status: prototype measured in QEMU; release line written but
off (`vars.TRUSTED_BOOT_RELEASE`).

## What it is

An extra download next to the normal ISOs, for people who want it. It is the
same desktop image, booted as a Kairos Trusted Boot system:

- systemd-boot loads one signed Unified Kernel Image (kernel, cmdline and the
  whole OS image as the initramfs). Secure Boot refuses anything not signed
  with our db key.
- Every boot stage is measured into the TPM. COS_OEM and COS_PERSISTENT
  (`/home`, `/var`, `/usr/local`) are LUKS2, and the key is sealed to a PCR
  policy signed with our PCR key. A disk moved to another machine, or booted
  with another kernel or cmdline, does not unlock.
- No GRUB, no A/B ext4 images on COS_STATE: the ESP holds four UKIs (active,
  passive, recovery, statereset).

The normal install stays the default. Nobody gets Trusted Boot without
choosing its ISO.

## How a Trusted Boot image is built from Hadron

Three things exist, and they are layers of one switch:

| Candidate | What it does |
|---|---|
| `ghcr.io/kairos-io/hadron-trusted:<ver>` | Hadron built with `BOOTLOADER=systemd`: the `full-image-pre-systemd` stage, the same as `hadron` minus dracut (147 MB vs 158 MB compressed for v0.6.4). Optional: dracut is simply unused on a UKI. |
| `kairos-init -t true` | The real switch. Skips the dracut initrd (`kairos-init/pkg/stages/steps_init.go:99`), removes `/boot/initrd*`, skips `/etc/cos/grub.cfg` (`steps_install.go:464`), writes `KAIROS_TRUSTED_BOOT="true"` to `/etc/kairos-release` (`steps_init.go:209`). On Hadron it installs no packages, so it is fast. |
| `TRUSTED_BOOT` build arg | Already in our Dockerfile `kairos` stage and passed as `-t "${TRUSTED_BOOT}"`. |

What our `kairos` stage had to change (done in this branch):

- The GRUB colour `sed` on `/etc/cos/grub.cfg` fails on a Trusted Boot build,
  which has no such file. It now runs only when `TRUSTED_BOOT != true`.
- `/etc/hadron-desktop/release` gets `TRUSTED_BOOT=<true|false>`, so
  hadron-update can tell which upgrade line the system is on.

The GRUB theme hooks (`09_hadron_grub_theme.yaml`, `10_hadron_grub_env.yaml`)
already skip `rd.immucore.uki` boots, and `kairos-user-live-only.sh` already
keeps the `kairos` user for `uki_install_mode`.

The ISO comes from `auroraboot build-uki -t iso` with the keys; the upgrade
image from `build-uki -t container` (an OCI image holding only
`EFI/kairos/norole.efi` and `EFI/BOOT/BOOTX64.EFI`).

### Cheap build path (no desktop rebuild)

`make UKI_FROM=ghcr.io/kairos-community/hadron-desktop:sway-vm-v0.0.8 uki-iso`
overrides the Dockerfile's `default` stage with the published image
(`--build-context default=docker-image://...`) and runs only the `kairos`
stage with `TRUSTED_BOOT=true`. Re-running kairos-init over a GRUB image works:
it removes the initrd and rewrites kairos-release. Costs: about 6 s for the
image, about 60 s for the ISO. Limit: changes to `rootfs/` in the tree are not
in the result, because they live in the `default` stage. Release builds use the
image the same release just pushed, so they are.

## Measured (sway-vm v0.0.8, QEMU/KVM, OVMF secboot, swtpm, 4 vCPU)

| What | Number |
|---|---|
| Rootfs in the UKI (`du -sx /`, after kairos-init -t true) | 974 MiB |
| One UKI (zstd level max) | 366 MiB (383,350,200 B) |
| ISO | 375 MiB |
| UKI upgrade image | 364 MiB |
| ESP after install | 4 UKIs = 1,463 MiB used, on a **15 GiB** ESP (agent default, `agent/pkg/config/spec.go:784`) |
| Key enrollment | systemd-boot `secure-boot-enroll if-safe` enrolled PK/KEK/db from the ISO on the first boot in setup mode, then rebooted. `bootctl`: "Secure Boot: enabled (user)", "Measured UKI: yes" |
| Live ISO boot, power-on to login | ~45 s (incl. the enroll reboot on the first run) |
| Installed boot, power-on to login prompt | 38-40 s, including systemd-boot's 5 s menu timeout; the kernel unpacks the 355 MB initrd in ~1 s |
| Install (`kairos-agent install`, UKI) | ~10-27 s to reboot: it copies 4 UKIs, it writes no images |
| `kairos-agent upgrade` from a local registry | 3.2 s; rebooted into the new active UKI, disk unlocked, session up |
| Desktop session | ly, then sway with waybar and the first-run app picker: screendumped on the installed system and after the upgrade |
| RAM, installed, session logged in, 8 GiB guest | Shmem 1,919 MiB; MemAvailable 5.36 GiB. `/` is a tmpfs with 943 MiB; the other ~1 GiB is the initramfs copy that stays in RAM, hidden under `/` |
| Same, with the immucore fix below | Shmem 977 MiB; MemAvailable 6.29 GiB (**~940 MiB given back**) |
| Lowest RAM that boots | 3 GiB boots, 2 GiB fails ("no space left on device" in immucore: the sysroot tmpfs is 50 % of RAM). 2.5 GiB boots with the fix; without it, not measured cleanly (see TPM lockout) |

Other images' rootfs (`du -sx /` of the published image, minus its 55 MiB
initrd): agent-v0.0.8 ~990 MiB, cosmic-vm-v0.0.8 ~1,880 MiB. The full GPU
lines (linux-firmware, more Mesa drivers) could not be measured: no
`<flavor>-v0.0.8` full tag was pullable.

Rule of thumb from the numbers: RAM held for the whole uptime is 2 x rootfs
today, 1 x rootfs with the fix. Boot needs RAM of at least ~2.6 x rootfs (the
tmpfs is capped at half of RAM, and the initramfs and its copy coexist during
the copy).

## Findings and blockers

1. **The OS image is held in RAM twice** (fixed in Kairos, PR below).
   AuroraBoot packs the whole rootfs into the UKI's initramfs
   (AuroraBoot `pkg/uki/uki.go:687-799`). immucore mounts a tmpfs on
   `/sysroot` (`immucore/pkg/state/steps_uki.go:205`), copies every top-level
   directory into it with `cp -a` (`:243`), moves it over `/` and chroots
   (`:312`), and never deletes the originals. The fix deletes each copied
   initramfs directory after the last copy, staying on one filesystem.
2. **The sysroot tmpfs has no size**, so it is 50 % of RAM
   (`steps_uki.go:205`). A machine with less than ~2.6 x rootfs RAM does not
   boot, with a red immucore failure screen. Not changed: the minimum stays
   ~2.5 GiB for sway. A `size=` would lower it, but at the cost of letting the
   tmpfs fill RAM; left for a Kairos discussion.
3. **15 GiB ESP by default** (`agent/pkg/config/spec.go:784`): on a 30 GiB
   disk, COS_PERSISTENT got 14.9 GiB. Four UKIs need 1.5 GiB, an upgrade
   rotation one more copy. Our installer should set
   `install.partitions.efi.size` to about 4096 MiB for a UKI install (room for
   five copies of a UKI up to ~800 MiB). Not done in this branch.
4. **The Hadron installer did not start on a UKI medium** (fixed here). On a
   UKI ISO, immucore writes `/run/cos/install_mode_interactive` and the cmdline
   has no `install-mode`, so `90_desktop_installer.yaml:14` never matched and
   the stock Kairos installer ran on tty1. A second stage now points
   `kairos-interactive.service` at `hadron-install-ui`.
5. **The installer could not write its answers** (fixed here). A UKI medium's
   root is read-only except `/etc /var /srv /home`; `hadron-install-ui` failed
   with "open /oem/99_hadron-user.yaml: read-only file system". The new
   `HADRON_CONFIG_OUT` (set by the UKI stage to
   `/etc/kairos/99_hadron-user.yaml`, a directory kairos-agent reads config
   from) fixes it. The installed system got the user, hostname and the ly
   login. `/etc/kairos` cannot be the scan dir too: kairos-init's
   `kairos-init-install-stage.yaml` there has an indented `install:` key that
   `ConfigPresent` would read as a seeded config.
6. **Installer progress steps are GRUB-shaped** (`install-ui/steps.go:36`
   matches "Generating grub files"). On UKI the bar skips that step. Not fixed.
7. **TPM dictionary-attack lockout after hard power-offs.** After a series of
   killed VMs, swtpm reported "TPM is in dictionary attack lock-out mode",
   kcrypt found no key and immucore halted the boot. A TPM 2.0 counts a
   non-orderly shutdown as a failed try when DA-protected objects are in use.
   swtpm's limits are lower than a laptop TPM's, but a user who holds the
   power button several times can hit it, and has only the recovery key
   path (none is set up today). The installer text must say so, and we need a
   recovery passphrase before shipping (the sibling encryption work).
8. **Upgrade images are separate.** A Trusted Boot system upgrades from the
   UKI container (`kairos-agent upgrade --source oci:<uki-image>`); the agent
   dumps the source onto the ESP and refuses a target without
   `EFI/kairos/norole.efi` and `/usr/bin/kairos` in its initrd
   (`agent/pkg/uki/upgrade.go:117`, `:321`). Our normal
   `<flavor>-<tag>` image must never be offered to it.
9. **Desktop switch / "install a custom image" do not apply.** Any image the
   user picks must be a UKI signed with the enrolled keys, so those
   hadron-update features are off on Trusted Boot.

## Keys

| Key | Where | Used for |
|---|---|---|
| `db.key`, `db.pem` | GitHub secrets `UKI_DB_KEY`, `UKI_DB_PEM` | signing every UKI |
| `tpm2-pcr-private.pem` | secret `UKI_TPM_PCR_KEY` | signing the PCR policy; the disk key is sealed to its public half |
| `PK.auth`, `KEK.auth`, `db.auth` | secrets `UKI_PK_AUTH` etc. (base64); public, could be in the repo | what systemd-boot enrolls in setup mode |
| `PK.key`, `KEK.key` | offline only, never in CI | signing future db/dbx updates |

Generated once with `auroraboot genkey` on an offline machine, with Microsoft
certs kept in KEK/db (the default) so option ROMs and dual-boot keep working.
CI writes them to `/dev/shm` with umask 077 and never to the workspace.

**Project keys vs user keys.** With project keys, our releases boot and
upgrade without the user doing anything except entering setup mode once; the
cost is that the security rests on our CI secrets. A user who wants their own
root of trust builds the ISO with `make uki-keys uki-iso` and must also build
every upgrade with the same keys: our `<flavor>-uki-<tag>` images would not
boot on their machine. That is documented, not automated, in the first
version. A db key rotation means a signed db update with KEK.key and a
transition release signed with both; out of scope for the first release.

Enrollment: on a machine in Secure Boot setup mode, systemd-boot enrolls the
`.auth` files from the ISO and reboots (measured). Most laptops need the user
to clear the keys in firmware setup first; on a machine that is not in setup
mode and already has Microsoft keys only, our UKI will not boot until our keys
are enrolled by hand (firmware "enroll from file" with `db.auth`). The
`secure-boot-enroll force` option exists but can brick firmware that needs
option ROMs signed by Microsoft; we keep `if-safe`.

## Release (`.github/workflows/release.yml`, job `build-uki`)

One line first: **sway, full GPU line**, because Trusted Boot is for real
machines with a TPM. The job needs `build`, runs only when the repository
variable `TRUSTED_BOOT_RELEASE` is `true`, and:

1. writes the keys from secrets to `/dev/shm/uki-keys`;
2. `make DESKTOP=sway GPU=full FIRMWARE=true UKI_FROM=$IMAGE_REPO:sway-<tag> uki-iso`
   (no desktop rebuild: only the kairos stage, ~1 min);
3. `build-uki -t container`, wrapped with the `io.kairos.hadron-desktop.*`
   labels plus `trusted-boot=true`, pushed and cosign-signed as
   `sway-uki-<tag>` (tested locally: the wrap works, 364 MiB);
4. uploads the ISO as artifact `uki-sway-uki`, deliberately not `release-*`,
   so the publish job's strict asset count and manifest checks are untouched.

Before it is switched on: add the ISO to the publish job (asset count,
manifest schema with a `trusted_boot` field, public-image check for
`<flavor>-uki-<tag>`), and a UKI size gate (fail above ~700 MiB, which keeps
five copies inside a 4 GiB ESP).

## hadron-update on a Trusted Boot system

- `LoadRelease` reads `TRUSTED_BOOT` (now written by the Dockerfile; ignored
  by today's hadron-update, so this branch is safe). When true, `Line()`
  returns `uki` and `lineTag` gives `<flavor>-uki-<tag>`.
- `Classify` trusts by name only `<flavor>-uki-<tag>` of a published release;
  by label, it requires `io.kairos.hadron-desktop.trusted-boot=true`. On a
  non-UKI system it refuses any image with that label (the agent would refuse
  it anyway, but the user should get the message from us).
- The tray hides "Switch desktop", "Switch to the hardware/virtual machine
  image" and "Install a custom image" on Trusted Boot, unless the other
  flavor has a published `-uki-` line.
- The backend stays `kairos-agent upgrade --source oci:<ref>`; the agent's
  UKI path is what we tested. "Boot the previous version" keeps working:
  `kairos-agent bootentry --select fallback` handles systemd-boot.

## Installer TUI

Shown only when `/run/cos/uki_install_mode` exists, as one extra screen
before the disk step:

> **Trusted Boot**
>
> This computer will only start Hadron Desktop releases signed by the Hadron
> project. Your files are encrypted, and the key is kept in this computer's
> TPM chip: the disk cannot be read in another computer.
>
> - Secure Boot must stay on. If you turn it off, or update the firmware in a
>   way that changes the measurements, the disk may not unlock.
> - Do not hold the power button to switch off: repeated hard power-offs can
>   lock the TPM for a while.
> - Hadron Desktop keeps its whole system in memory: this edition needs at
>   least 4 GB of RAM (8 GB for COSMIC).
> - You cannot switch to another desktop or to the normal edition by an
>   update. To leave Trusted Boot, reinstall from the normal ISO.
>
> Enter to continue, Esc to go back.

The TUI also checks before it starts: a TPM 2.0 (`/sys/class/tpm/tpm0`),
Secure Boot in user mode (`bootctl status` or the SecureBoot EFI variable), and
`MemTotal`; it refuses with a plain reason when one is missing. It sets
`install.partitions.efi.size: 4096` in the cloud-config.

## Go / no-go per variant

| Variant | Rootfs | Verdict |
|---|---|---|
| sway (vm line, measured) | 974 MiB | **Go** for 4 GiB+ machines. Boots at 3 GiB today, 2.5 GiB with the immucore fix; 2 GiB fails. Desktop, install, upgrade all worked. |
| sway (full line, first release line) | not measured | **Go, conditional**: measure the UKI in the first `build-uki` run; ship only if the UKI is under ~700 MiB and the RAM rule (2.6 x rootfs to boot) gives 4-6 GiB. |
| i3, niri | not measured (niri-vm 543 MB compressed vs sway 487) | **Go, conditional** on the same size gate, after sway. |
| cosmic | ~1,880 MiB | **No-go without the immucore fix** (3.8 GiB held for the uptime). With it: go for **8 GiB+** only. |
| agent | ~990 MiB | **No-go / not applicable**: a VM appliance, where a TPM-sealed Secure Boot chain buys little and the agent gates boot with plain OVMF. |

Overall: go for an opt-in sway Trusted Boot ISO once (a) the immucore fix is
released, (b) the installer sets a 4 GiB ESP and warns as above, and (c) a
recovery passphrase exists for the TPM-lockout and firmware-update cases.

## Not tested

- The full GPU lines, real hardware, real firmware enrollment (only OVMF).
- A non-UKI boot of the same image for a boot-time comparison.
- The `build-uki` release job itself (written, linted, not run; it needs the
  secrets).
- hadron-update following the `-uki-` line (designed above, not coded); the
  upgrade was run with `kairos-agent` directly.
- The interactive TUI screens on the UKI medium (driven through the
  non-interactive environment, which uses the same output path).
