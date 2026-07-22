#!/usr/bin/env bash
# Assert the splash is baked into the image's initramfs.
#
#   test/boot-ux/check-initramfs.sh [IMAGE]
#
# The overlay-file checks are cheap sanity; the initramfs content checks are the
# ones that matter. Three known failure modes make the module silently no-op:
#   1. the splash binary landing in the image AFTER kairos-init runs `dracut -f`
#      -> initramfs has no splash at all
#   2. buildx caching the kairos-init stage with the module absent
#      -> initramfs has no splash at all
#   3. buildx serving a CACHED kairos stage built against an OLDER splash binary
#      -> initramfs has a splash, at the right path, that is the wrong build
# (1) and (2) leave the overlay files present and the initramfs without the
# splash, and a path grep catches them. (3) is invisible to any path-based check:
# every such check still passes while the shipped animation is stale. That is the
# failure the Makefile's `--no-cache-filter hadron-splash,kairos` guards against,
# so it is checked here by content — the sha256 of the initramfs copy must equal
# the sha256 of the binary in the image.
#
# NOTE: the chroot-based exec check below requires root INSIDE the container. It
# will fail under `docker run --user` or on a rootless / userns-remapped daemon,
# where chroot(2) is not permitted. That is an environment limitation, not a
# regression in the image.
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
# .wants, not .requires: a failed splash must not be able to stall initrd.target.
# See the rationale in the dracut module's install().
check "initramfs unit activated via initrd.target.wants" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'initrd.target.wants/hadron-splash.service'"
check "initramfs unit NOT wired as a hard initrd.target.requires dependency" \
    "! lsinitrd /boot/initrd 2>/dev/null | grep -q 'initrd.target.requires/hadron-splash.service'"
# Failure mode (3) from the header: a cached kairos stage ships a stale splash at
# the correct path. Only comparing content catches it.
check "initramfs splash is the same build as the image's (not a stale cached one)" \
    "mkdir -p /tmp/ird-sha && cd /tmp/ird-sha && lsinitrd --unpack /boot/initrd && \
     a=\$(sha256sum /usr/bin/hadron-splash | cut -d' ' -f1) && \
     b=\$(sha256sum /tmp/ird-sha/usr/bin/hadron-splash | cut -d' ' -f1) && \
     echo \"image : \$a\" && echo \"initrd: \$b\" && [ \"\$a\" = \"\$b\" ]"
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
