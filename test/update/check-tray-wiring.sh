#!/usr/bin/env bash
# The checks are strings that the inner sh expands ($R, globs), on purpose.
# shellcheck disable=SC2016
# Assert the update tray is wired into a variant: the root update service and
# its polkit policy (with the system extensions action, and the hadron-ext
# link) are installed, polkitd can run, and the session starts
# `hadron-update tray` (and, on i3, the snixembed tray bridge). For the agent
# image, assert the opposite: no update service and nothing that starts
# hadron-update.
#
#   test/update/check-tray-wiring.sh IMAGE FLAVOR
#   test/update/check-tray-wiring.sh --root DIR FLAVOR
#
# FLAVOR is sway, i3, cosmic, niri or agent. With --root the checks read a
# directory tree instead of an image (for example the rootfs/ and
# rootfs-<variant>/ overlays plus stage output), so the wiring can be tested
# without building an image. Checks that must run a program or read the
# image's users (ldd, /etc/passwd written by the build) need an image and are
# reported as SKIP under --root.
set -euo pipefail

usage() { echo "usage: $0 IMAGE FLAVOR | --root DIR FLAVOR" >&2; exit 2; }

ROOT=""
IMAGE=""
if [ "${1:-}" = "--root" ]; then
    [ $# -eq 3 ] || usage
    ROOT="$(cd "$2" && pwd)"
    FLAVOR="$3"
else
    [ $# -eq 2 ] || usage
    IMAGE="$1"
    FLAVOR="$2"
fi
case "$FLAVOR" in sway|i3|cosmic|niri|agent) ;; *) usage ;; esac

fail=0
# check DESC CMD: CMD runs under sh with $R set to the root to read ("" in an
# image), so every path in it is written "$R/...".
check() {
    local desc="$1"; shift
    local ok
    if [ -n "$IMAGE" ]; then
        docker run --rm -e R= "$IMAGE" sh -c "$*" >/dev/null 2>&1 && ok=1 || ok=0
    else
        R="$ROOT" sh -c "$*" >/dev/null 2>&1 && ok=1 || ok=0
    fi
    if [ "$ok" -eq 1 ]; then echo "PASS: $desc"; else echo "FAIL: $desc"; fail=1; fi
}
# check_image DESC CMD: like check, but only meaningful inside a built image.
check_image() {
    if [ -n "$IMAGE" ]; then check "$@"; else echo "SKIP: $1 (needs an image)"; fi
}
# no_missing_libs DESC PATH: PATH exists and every library it links resolves.
# Guarded on the file existing: a missing file also prints no "not found".
no_missing_libs() {
    check_image "$1" "test -x $2 && ldd $2 >/tmp/ldd 2>&1 && ! grep -q 'not found' /tmp/ldd"
}

# The session configuration that may start programs, per flavor. Globs expand
# inside the check, against $R. Comment lines are dropped before matching.
case "$FLAVOR" in
    sway)        SESSION='$R/etc/sway/config $R/etc/sway/config.d/*' ;;
    i3|agent)    SESSION='$R/etc/i3/config $R/etc/i3/config.d/*' ;;
    niri)        SESSION='$R/etc/niri/config.kdl' ;;
    cosmic)      SESSION='$R/etc/xdg/autostart/*.desktop' ;;
esac
# Every place any variant could start the updater from, for the negative checks.
ALL_SESSION='$R/etc/sway/config $R/etc/sway/config.d/* $R/etc/i3/config $R/etc/i3/config.d/* $R/etc/niri/config.kdl $R/etc/xdg/autostart/*.desktop'
lines() { echo "cat $1 2>/dev/null | grep -Ev '^[[:space:]]*(#|//)'"; }

SYSTEM_FILES="
/usr/share/dbus-1/system.d/io.kairos.HadronUpdate1.conf
/usr/share/dbus-1/system-services/io.kairos.HadronUpdate1.service
/usr/lib/systemd/system/hadron-updated.service
/usr/share/polkit-1/actions/io.kairos.hadron-update.policy
/usr/share/polkit-1/rules.d/50-hadron-update.rules"

# The release file names the image line upgrades follow (GPU=full or vm), and
# the image says the same in its label; the agent is always vm. Written by the
# Dockerfile kairos stage, so only an image has it.
if [ -n "$IMAGE" ]; then
    gpu_label="$(docker image inspect --format '{{index .Config.Labels "io.kairos.hadron-desktop.gpu"}}' "$IMAGE" 2>/dev/null || true)"
    if [ "$FLAVOR" = agent ]; then want_gpu='vm'; else want_gpu='(full|vm)'; fi
    if [[ "$gpu_label" =~ ^${want_gpu}$ ]]; then echo "PASS: the image is labelled on the $gpu_label line"; else echo "FAIL: the image is labelled on the line '$gpu_label', want $want_gpu"; fail=1; fi
    check "the release file names the same line" "grep -qx 'GPU=$gpu_label' /etc/hadron-desktop/release"
else
    echo "SKIP: the image line (needs an image)"
fi

if [ "$FLAVOR" = agent ]; then
    for f in $SYSTEM_FILES; do
        check "agent: no $f" "! test -e \$R$f"
    done
    check "agent: no hadron-ext (system extensions)" "! test -e \$R/usr/bin/hadron-ext"
    check "agent: no hadron-recovery (disk recovery key)" "! test -e \$R/usr/bin/hadron-recovery"
    check "agent: no setting that merges unsigned extensions" \
        "! test -e \$R/etc/kairos/90_hadron_extensions.yaml && ! test -e \$R/system/oem/93_hadron_extensions.yaml"
    check "agent: no unit starts hadron-update" \
        "! grep -rqs 'hadron-update' \$R/usr/lib/systemd/system \$R/etc/systemd/system"
    check "agent: the session config exists" 'test -f $R/etc/i3/config'
    check "agent: nothing in the session starts hadron-update" \
        "! { $(lines "$ALL_SESSION"); } | grep -q 'hadron-update'"
    check "agent: the session does not start snixembed" \
        "! { $(lines "$ALL_SESSION"); } | grep -q 'snixembed'"
    echo
    [ "$fail" -eq 0 ] && echo "tray-wiring ($FLAVOR): ALL PASS" || echo "tray-wiring ($FLAVOR): FAILURES"
    exit "$fail"
fi

# The root update service: policy, activation, unit, polkit action and rule.
for f in $SYSTEM_FILES; do
    check "$f installed" "test -f \$R$f"
done
check "D-Bus activation starts hadron-updated.service" \
    'grep -qx "SystemdService=hadron-updated.service" $R/usr/share/dbus-1/system-services/io.kairos.HadronUpdate1.service'
check_image "hadron-update is installed" 'test -x /usr/bin/hadron-update'
check "the polkit policy declares the extensions action" \
    'grep -q "<action id=\"io.kairos.hadron-update.extensions\">" $R/usr/share/polkit-1/actions/io.kairos.hadron-update.policy'
check_image "hadron-ext is a link to hadron-update" 'test "$(readlink /usr/bin/hadron-ext)" = hadron-update'
check "the polkit policy declares the recovery action" \
    'grep -q "<action id=\"io.kairos.hadron-update.recovery\">" $R/usr/share/polkit-1/actions/io.kairos.hadron-update.policy'
check_image "hadron-recovery is a link to hadron-update" 'test "$(readlink /usr/bin/hadron-recovery)" = hadron-update'

# polkitd answers the daemon's checks. It is D-Bus activated, so it needs its
# activation file and unit, not an enable link.
check "polkitd installed" 'test -x $R/usr/lib/polkit-1/polkitd'
check "polkitd is D-Bus activated" \
    'grep -qx "SystemdService=polkit.service" $R/usr/share/dbus-1/system-services/org.freedesktop.PolicyKit1.service'
check "polkit.service unit installed" 'test -f $R/usr/lib/systemd/system/polkit.service'
check "polkitd may own its bus name" 'test -f $R/usr/share/dbus-1/system.d/org.freedesktop.PolicyKit1.conf'
no_missing_libs "polkitd links resolve" /usr/lib/polkit-1/polkitd
check_image "polkitd system user exists" 'grep -q "^polkitd:" /etc/passwd'
check_image "polkit rules directory is owned by polkitd" \
    'test "$(stat -c %U /usr/share/polkit-1/rules.d)" = polkitd'

# The session starts the tray, and no longer the old notifier.
case "$FLAVOR" in
    sway|i3)
        TRAY='^[[:space:]]*exec(_always)?[[:space:]]+(--no-startup-id[[:space:]]+)?hadron-update[[:space:]]+tray([[:space:]]|$)' ;;
    niri)
        TRAY='^[[:space:]]*spawn-at-startup[[:space:]]+"hadron-update"[[:space:]]+"tray"[[:space:]]*$' ;;
    cosmic)
        TRAY='^Exec=hadron-update tray$' ;;
esac
check "the $FLAVOR session starts hadron-update tray" \
    "{ $(lines "$SESSION"); } | grep -Eq '$TRAY'"
check "nothing starts hadron-update notify" \
    "! { $(lines "$ALL_SESSION"); } | grep -Eq 'hadron-update(\"[[:space:]]+\"|[[:space:]]+)notify'"

if [ "$FLAVOR" = i3 ]; then
    # i3bar is an XEmbed tray only; snixembed shows the tray's
    # StatusNotifierItem in it.
    check "snixembed installed" 'test -x $R/usr/bin/snixembed'
    no_missing_libs "snixembed links resolve" /usr/bin/snixembed
    check "the i3 session starts snixembed --fork" \
        "{ $(lines "$SESSION"); } | grep -Eq '^[[:space:]]*exec[[:space:]]+(--no-startup-id[[:space:]]+)?snixembed[[:space:]]+--fork'"
    # Cosmetic only: i3 does not wait for --fork, and the tray registers
    # with a watcher that appears later. Listed first, the icon shows sooner.
    check "snixembed is listed before the tray (cosmetic ordering)" \
        "{ $(lines "$SESSION"); } | grep -E 'snixembed|hadron-update[[:space:]]+tray' | head -n1 | grep -q snixembed"
    check "i3bar shows a tray" \
        "{ $(lines "$SESSION"); } | grep -q '^[[:space:]]*bar[[:space:]]*{' && ! { $(lines "$SESSION"); } | grep -Eq '^[[:space:]]*tray_output[[:space:]]+none'"
fi

echo
[ "$fail" -eq 0 ] && echo "tray-wiring ($FLAVOR): ALL PASS" || echo "tray-wiring ($FLAVOR): FAILURES"
exit "$fail"
