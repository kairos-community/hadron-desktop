#!/bin/bash
# Hadron: the boot splash is the password agent in the initramfs. See
# splash/README.md ("Disk passphrase") and hadron-ask-gate.sh.

# Only for Hadron's own splash. kairos-init's stock /usr/bin/kairos-splash is
# a link to the kairos multi-call binary, which does not speak the agent
# protocol and does not send READY=1, so the Type=notify drop-in would fail it.
check() {
    require_binaries /usr/bin/kairos-splash || return 1
    [[ -L ${dracutsysrootdir-}/usr/bin/kairos-splash ]] && return 1
    return 0
}

depends() {
    echo systemd kairos-splash
    return 0
}

install() {
    declare systemdsystemunitdir=${systemdsystemunitdir}
    declare moddir=${moddir}
    declare initdir=${initdir}

    inst_multiple sleep systemctl
    inst_script "${moddir}/hadron-ask-gate.sh" /usr/libexec/hadron-ask-gate

    mkdir -p "${initdir}/${systemdsystemunitdir}/kairos-splash.service.d"
    inst_simple "${moddir}/kairos-splash-agent.conf" \
        "${systemdsystemunitdir}/kairos-splash.service.d/20-hadron-agent.conf"

    inst_multiple -o "${systemdsystemunitdir}/systemd-ask-password-console.path" \
        "${systemdsystemunitdir}/systemd-ask-password-console.service"
    mkdir -p "${initdir}/${systemdsystemunitdir}/systemd-ask-password-console.service.d"
    inst_simple "${moddir}/ask-password-console-gate.conf" \
        "${systemdsystemunitdir}/systemd-ask-password-console.service.d/20-hadron-ask-gate.conf"
}
