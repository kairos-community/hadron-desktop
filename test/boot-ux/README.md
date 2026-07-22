# Boot UX checks

Fast, non-booting assertions for the splash, initramfs, branding and ISO
overlay. Run against an already-built image:

    make image
    test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
    test/boot-ux/run.sh i3-desktop:dev   'i3 · xlibre · kairos'

Or via make, which runs the suite for whichever variants are already built:

    make check-boot-ux

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
