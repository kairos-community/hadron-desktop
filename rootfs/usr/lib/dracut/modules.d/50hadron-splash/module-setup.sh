#!/bin/bash
# hadron-desktop early-boot splash — dracut module.
#
# Paints the animated splash from inside the initramfs (on tty1), so the
# animation starts at early boot instead of only just before the login prompt.
# Structure mirrors kairos-init's own 28immucore module (the canonical pattern
# for adding a binary + a systemd unit to the Kairos initramfs).
#
# The unit dies at switch-root (Conflicts=initrd-switch-root.target); the booted
# system's hadron-splash.service (ordered Before=ly@tty1.service) then continues
# the splash. `quiet` on the kernel cmdline keeps the hand-off gap clean.
#
# hadron-splash is a dynamic musl binary, so inst_multiple is used (like
# immucore) to pull the binary AND its shared-library deps into the initramfs.
# inst_simple would copy the ELF without its libs and it would fail to exec.

# check() decides whether dracut pulls this module in. 0 = include in the default
# set; the /etc/dracut.conf.d drop-in also force-adds it via add_dracutmodules.
check() {
    require_binaries /usr/bin/hadron-splash || return 1
    return 0
}

# No dependencies on other dracut modules.
depends() {
    return 0
}

install() {
    inst_multiple /usr/bin/hadron-splash
    inst_simple "${moddir}/hadron-splash.service" "${systemdsystemunitdir}/hadron-splash.service"
    # Activate the unit inside the initramfs by wiring it into initrd.target, the
    # same way 28immucore activates immucore.service.
    mkdir -p "${initdir}/${systemdsystemunitdir}/initrd.target.requires"
    ln_r "../hadron-splash.service" "${systemdsystemunitdir}/initrd.target.requires/hadron-splash.service"

    # Drop-in that takes immucore's logs off the console so they don't print over
    # the splash. 28immucore installs immucore.service into the same initdir;
    # this only adds a .d override, so module ordering does not matter.
    mkdir -p "${initdir}/${systemdsystemunitdir}/immucore.service.d"
    inst_simple "${moddir}/immucore-quiet.conf" "${systemdsystemunitdir}/immucore.service.d/10-quiet.conf"
}
