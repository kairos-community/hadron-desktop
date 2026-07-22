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
# The splash is a DYNAMICALLY linked musl binary: without its loader in the
# initramfs it cannot exec at all, and `quiet` hides the failure entirely (it
# would surface only as "no animation" on a real boot). The module uses
# inst_multiple precisely so dracut resolves the ELF's deps. A plain grep for
# the loader is not proof — the base initramfs already carries musl for
# immucore — so unpack the initramfs and actually exec the splash inside it via
# chroot. With stdout not a tty the splash prints its version banner and exits 0,
# which only happens if the loader and libc resolved.
check "splash execs inside the unpacked initramfs (musl loader + libc resolve)" \
    "mkdir -p /tmp/ird && cd /tmp/ird && lsinitrd --unpack /boot/initrd && chroot . /usr/bin/hadron-splash | grep -q HADRON"
check "immucore quiet drop-in baked into the initramfs" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'immucore.service.d/10-quiet.conf'"

exit "$fail"
