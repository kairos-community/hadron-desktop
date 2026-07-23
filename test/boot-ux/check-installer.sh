#!/usr/bin/env bash
# Assert the installer TUI is present and wired up in a built image.
#
#   test/boot-ux/check-installer.sh [IMAGE]
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

check "hadron-install-ui present in /usr/bin" 'test -x /usr/bin/hadron-install-ui'
# Both of the next two are guarded on the binary EXISTING. Written the obvious
# way ("! ldd ... | grep -q 'not found'", "! bin --nonsense") they pass against
# an image with no binary at all -- a missing file also produces no "not found"
# line and also exits non-zero. Statically: no PT_INTERP, so the ELF carries no
# dynamic-loader path; a dynamically linked build embeds "/lib64/ld-linux-*".
check "hadron-install-ui is static (no interpreter needed)" \
    'test -x /usr/bin/hadron-install-ui && ! grep -qa "ld-linux" /usr/bin/hadron-install-ui'
check "rejects unknown arguments with exit 2" \
    '/usr/bin/hadron-install-ui --nonsense >/dev/null 2>&1; [ $? -eq 2 ]'
check "openssl available for password hashing" 'command -v openssl'
check "old shell installer removed" '! test -e /usr/local/bin/hadron-install'
check "installer drop-in points at hadron-install-ui" \
    'grep -q "/usr/bin/hadron-install-ui" /system/oem/90_desktop_installer.yaml'
check "drop-in keeps the install-mode guard" \
    'grep -q "install-mode" /system/oem/90_desktop_installer.yaml'

exit "$fail"
