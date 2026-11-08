#!/bin/bash
# Ask for the disk passphrase at boot. Runs in the initramfs, before immucore.
#
# The installer's "Encrypt your files" choice encrypts COS_PERSISTENT with
# kcrypt's passphrase method (kairos-io/kairos#6022): a LUKS2 container with
# the subsystem "kcrypt:passphrase" in its header, that only the passphrase
# the user chose can open. This script finds that container, asks for the
# passphrase with systemd-ask-password until it is right (the boot splash
# answers, or the console agent when there is no framebuffer; see
# 51hadron-askpass), and opens the container under the
# mapper name kcrypt uses (/dev/mapper/<kernel name>). kcrypt then finds the
# partition already open and skips it (sdk/kcrypt/unlock.go,
# findEncryptedPartitions), and the boot goes on.
#
# It waits for as long as it takes: nothing else can open the disk, so a
# timeout could only end in a failed boot.
#
# Any other container (Trusted Boot's TPM-sealed key, a key server) is left to
# immucore, which has its own fallback prompt (kcrypt.passphrase_fallback).
#
# Kernel command line:
#   hadron.unlock=0   never ask here (kcrypt asks instead)

set -u

PASSPHRASE_SUBSYSTEM="kcrypt:passphrase"

log() {
    echo "hadron-unlock: $*" >&2
}

cmdline_value() {
    local key=$1 word words
    read -r -a words < /proc/cmdline
    for word in "${words[@]}"; do
        case "$word" in
            "$key="*) echo "${word#*=}"; return 0 ;;
        esac
    done
    return 1
}

# Encrypted persistent partitions that are not open yet. kcrypt labels the LUKS
# container COS_PERSISTENT_LUKS (older installs: COS_PERSISTENT) and the GPT
# partition "persistent".
locked_persistent_devices() {
    local dev label partlabel
    for dev in $(blkid -o device -t TYPE=crypto_LUKS 2>/dev/null); do
        label=$(blkid -o value -s LABEL "$dev" 2>/dev/null)
        partlabel=$(blkid -o value -s PARTLABEL "$dev" 2>/dev/null)
        case "$label" in
            COS_PERSISTENT|COS_PERSISTENT_LUKS) ;;
            *) [ "$partlabel" = persistent ] || continue ;;
        esac
        [ -e "/dev/mapper/${dev##*/}" ] && continue
        echo "$dev"
    done
}

subsystem_of() {
    cryptsetup luksDump "$1" 2>/dev/null | sed -n 's/^Subsystem:[[:space:]]*//p'
}

# has_recovery_key DEVICE: the container has a recovery key, a keyslot that a
# systemd-recovery token claims (the installer's "Also create a recovery
# key?", or `hadron-recovery new-key`).
has_recovery_key() {
    cryptsetup luksDump "$1" 2>/dev/null |
        sed -n '/^Tokens:/,/^Digests:/p' | grep -q '^ *[0-9][0-9]*: systemd-recovery$'
}

# recovery_key_form TEXT: TEXT as a recovery key in its canonical form (lower
# case, eight dash-separated groups of eight), or nothing when TEXT is not 64
# modhex letters once spaces and dashes are dropped. People type the key in
# any case and often without its dashes; the keyslot holds the dashes.
recovery_key_form() {
    local s
    s=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d ' -')
    [[ $s =~ ^[cbdefghijklnrtuv]{64}$ ]] || return 1
    printf '%s' "$s" | sed 's/.\{8\}/&-/g; s/-$//'
}

# open_with DEVICE NAME TEXT: opens DEVICE with TEXT as typed, then, when it
# may be a recovery key, in the key's canonical form.
open_with() {
    local dev=$1 name=$2 pass=$3 key
    printf '%s' "$pass" | cryptsetup open --key-file=- "$dev" "$name" 2>/dev/null && return 0
    [ "${4:-0}" = 1 ] || return 1
    key=$(recovery_key_form "$pass") || return 1
    [ "$key" != "$pass" ] || return 1
    printf '%s' "$key" | cryptsetup open --key-file=- "$dev" "$name" 2>/dev/null
}

# ask_and_open DEVICE: asks until the passphrase opens DEVICE.
ask_and_open() {
    local dev=$1 name=${1##*/} prompt wrong pass tries=0 recovery=0
    # No device name in the prompt: the person knows "the disk".
    prompt="Enter the disk passphrase"
    wrong="Wrong passphrase, try again"
    if has_recovery_key "$dev"; then
        recovery=1
        prompt="Enter the disk passphrase or recovery key"
        wrong="Wrong passphrase or recovery key, try again"
    fi
    while :; do
        # --no-tty: this unit has no terminal, so the request goes to the
        # password agents: kairos-splash draws the prompt, or, without a
        # framebuffer, the console agent asks on /dev/console
        # (51hadron-askpass decides which). The splash shows the Id's retry
        # as "Wrong passphrase"; the device in the prompt is for the console.
        # --timeout=0: no timeout.
        if ! pass=$(systemd-ask-password --no-tty --timeout=0 \
                --id="hadron-unlock:${name}" --icon=drive-harddisk "$prompt"); then
            log "the passphrase prompt for ${dev} failed; leaving it to immucore"
            return 1
        fi
        tries=$((tries + 1))
        if open_with "$dev" "$name" "$pass" "$recovery"; then
            log "opened ${dev} as /dev/mapper/${name} after ${tries} attempt(s)"
            return 0
        fi
        log "wrong passphrase for ${dev} (attempt ${tries})"
        prompt=$wrong
    done
}

main() {
    local dev

    if [ "$(cmdline_value hadron.unlock || true)" = 0 ]; then
        log "disabled on the kernel command line"
        return 0
    fi

    for dev in $(locked_persistent_devices); do
        if [ "$(subsystem_of "$dev")" != "$PASSPHRASE_SUBSYSTEM" ]; then
            log "${dev} is not a passphrase disk; leaving it to immucore"
            continue
        fi
        ask_and_open "$dev" || true
    done
    return 0
}

main "$@"
