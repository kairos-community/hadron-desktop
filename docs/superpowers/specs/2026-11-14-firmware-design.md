# Firmware for the full line

## Problem

The full line (`GPU=full FIRMWARE=true`) is the image for real machines, but
v0.0.10 does not work on many of them:

- **No Intel Wi-Fi firmware.** The image has `intel/iwlwifi/*.ucode`, but
  iwlwifi asks for `iwlwifi-<card>-<api>.ucode` at the top of
  `/usr/lib/firmware`. linux-firmware declares that name as a `Link:` in
  WHENCE. `tools/install-firmware.sh` made only the links whose own path was
  under an installed prefix, so it made none of these: 0 of the 43 names the
  kernel's iwlwifi declares were present. The same bug dropped `mt7601u.bin`,
  `mt7662*.bin` (MediaTek USB) and some `brcm/brcmfmac*-sdio` links.
- **No CPU microcode.** Neither `intel-ucode` nor `amd-ucode` was in the
  image, so dracut built no early cpio. Hadron's kernel has
  `CONFIG_MICROCODE_LATE_LOADING` off, so the initramfs is the only place the
  kernel can load an update.
- **No SOF audio firmware.** Intel laptops since Skylake (and every Meteor
  Lake and later) run the audio DSP with Sound Open Firmware. It is not in
  linux-firmware; it is released as sof-bin. Without it most of these laptops
  have no sound. Laptops with Cirrus or TI speaker amplifiers also need their
  tuning files.
- **Too much of the rest.** About 520 MB of blobs, of which 72 MB are Marvell
  Prestera switch firmware, 20 MB MediaTek access-point firmware, and most of
  the 109 MB of iwlwifi are API versions the kernel never loads.

## Decision

Only the full line gets firmware. The vm line gets none: a VM has no device
that loads firmware, and the kernel does not apply microcode under a
hypervisor.

The set (Dockerfile `firmware` stage): amdgpu, radeon, i915, xe, nouveau GSP
(only the GSP-RM release the kernel prefers, checked at build time); iwlwifi
(2 newest APIs per card) and Intel BT; ath9k_htc, ath10k QCA6174/QCA9377/
QCA988X, ath11k WCN6855/QCA6390/QCA2066, ath12k WCN7850/QCC2072, qca; rtw88,
rtw89, rtlwifi, rtl_bt, rtl_nic; MediaTek MT7921/22/25/27 and MT76x0/x2/
7601U/7615/7663; brcmfmac and cypress; Marvell PCIe (Surface); Cirrus and TI
speaker amplifiers; amd-ucode. From other sources: sof-bin (Intel SOF, without
the logger dictionaries) and Intel's microcode release. All pinned, checked
with SHA256, fetched with retries, each in its own RUN so a bump re-downloads
one source.

Left out: pre-Turing nvidia blobs (they only accelerate; Mesa here has no
NVIDIA driver), nouveau GSP 535, router and AP Wi-Fi, Arm SoC and Chromebook
firmware, Marvell switch and SDIO firmware, server NICs and HBAs, TV tuners,
NPUs, Intel IPU cameras.

No GPU firmware in the initramfs: kairos-init puts only simpledrm, bochs and
virtio-gpu there, and the splash and passphrase prompt draw on simpledrm.
amdgpu and i915 load after switch_root, from `/usr/lib/firmware`.

## How it is kept right

- `install-firmware.sh` makes every WHENCE link whose target it installed,
  wherever the link is.
- `--require` reads the `firmware=` names of every module in the base image
  and fails the build if one that the chosen parts provide is not installed.
  On v0.6.4 it checks 1328 names.
- `test/firmware/check-install-firmware.sh` runs in CI on a fake tree.

## Size

| | Blobs in the image | sway full ISO |
| --- | --- | --- |
| v0.0.10 | about 520 MB | 1076.6 MB |
| this change | about 290 MB | 887.4 MB |

All of linux-firmware with zstd is about 960 MB. The initramfs grows from
59.1 MB to 78.6 MB: the uncompressed early cpio holds 19.2 MB of Intel and
0.3 MB of AMD microcode.
