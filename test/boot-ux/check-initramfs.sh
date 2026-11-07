#!/usr/bin/env bash
# Assert our splash is baked into the image's initramfs.
#
#   test/boot-ux/check-initramfs.sh [IMAGE]
#
# kairos-init's 50kairos-splash dracut module copies /usr/bin/kairos-splash
# (with its libraries) and every file in /etc/kairos/branding/splash into the
# initramfs. Two failure modes leave every image-side check green:
#   1. BuildKit serving a cached kairos stage, so the initramfs carries an older
#      splash than the image (the Makefile's --no-cache-filter guards this);
#   2. the module's check() refusing (no /usr/bin/kairos-splash), so the
#      initramfs has no splash at all.
# Both are caught here by looking inside /boot/initrd, the first by content.
#
# NOTE: the chroot-based exec check requires root INSIDE the container. It fails
# under `docker run --user` or on a rootless / userns-remapped daemon, where
# chroot(2) is not permitted. That is an environment limitation, not a
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

B=etc/kairos/branding/splash
check "initrd exists" "test -f /boot/initrd"
check "splash baked into the initramfs" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'usr/bin/kairos-splash'"
check "release version baked into the initramfs" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q '$B/version'"
# .wants, not .requires: a failed splash must not be able to stall initrd.target.
check "initramfs unit activated via initrd.target.wants" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'initrd.target.wants/kairos-splash.service'"
check "initramfs unit NOT wired as a hard initrd.target.requires dependency" \
    "! lsinitrd /boot/initrd 2>/dev/null | grep -q 'initrd.target.requires/kairos-splash.service'"
check "immucore quiet drop-in baked into the initramfs" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'immucore.service.d/10-quiet.conf'"
check "no leftover hadron-splash in the initramfs" \
    "! lsinitrd /boot/initrd 2>/dev/null | grep -q 'hadron-splash'"
# The splash answers disk passphrase questions (51hadron-askpass). The console
# agent must wait for it, and the keymaps must be there for the passphrase.
check "splash unit is Type=notify (agent decision before the console agent)" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'kairos-splash.service.d/20-hadron-agent.conf'"
check "console password agent gated on the splash" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'systemd-ask-password-console.service.d/20-hadron-ask-gate.conf' && \
     lsinitrd /boot/initrd 2>/dev/null | grep -q 'usr/libexec/hadron-ask-gate'"
check "no drop-in that stops the splash when the console agent starts" \
    "! lsinitrd /boot/initrd 2>/dev/null | grep -q 'systemd-ask-password-console.service.d/10-hadron-splash.conf'"
check "console keymaps and loadkeys in the initramfs (rd.vconsole.keymap=)" \
    "lsinitrd /boot/initrd 2>/dev/null | grep -q 'keymaps/i386/qwertz/de-latin1' && \
     lsinitrd /boot/initrd 2>/dev/null | grep -q 'usr/bin/loadkeys'"
# Failure mode (1): a stale splash at the right path. Only content catches it.
# sha256sum, not cmp: the image has no diffutils, and a missing tool would read
# as a mismatch.
check "initramfs splash is the image's build (not a stale cached one)" \
    "mkdir -p /tmp/ird-sha && cd /tmp/ird-sha && lsinitrd --unpack /boot/initrd && \
     a=\$(sha256sum < /usr/bin/kairos-splash) && b=\$(sha256sum < /tmp/ird-sha/usr/bin/kairos-splash) && \
     [ \"\$a\" = \"\$b\" ]"
# Exec it inside the unpacked initramfs: proves the binary and its musl loader
# resolve there, and that it reads the version file there.
check "splash execs inside the unpacked initramfs" \
    "mkdir -p /tmp/ird && cd /tmp/ird && lsinitrd --unpack /boot/initrd && chroot . /usr/bin/kairos-splash 2>/dev/null | grep -q 'HADRON v'"

exit "$fail"
