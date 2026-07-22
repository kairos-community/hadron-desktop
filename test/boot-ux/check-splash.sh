#!/usr/bin/env bash
# Assert the customized splash is present and sane in a built desktop image.
#
#   test/boot-ux/check-splash.sh [IMAGE]
#
# Runs the splash with stdout redirected to a pipe (not a tty), which takes the
# fallback path in main() — that is both a brand check and a crash check, since
# the whole program is linked and the version reader runs before the fallback.
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

check "hadron-splash present and executable" 'test -x /usr/bin/hadron-splash'
check "hadron-splash non-tty fallback prints HADRON" '/usr/bin/hadron-splash | grep -q HADRON'
# `strings` is not present in the image, so scan the binary with `grep -a`
# instead — otherwise this check passes vacuously on a failed `strings`.
check "no stale AIOS branding in the binary" '! grep -aiq aios /usr/bin/hadron-splash'
# Discriminating check: the base image ships its own HADRON-branded splash whose
# tagline is "the foundation for image-based systems". Only OUR build carries
# the hadron-desktop tagline, so this is what proves the COPY actually landed.
check "binary is our rebranded build, not the stock one" \
    'grep -aq "the immutable tiling desktop" /usr/bin/hadron-splash'

exit "$fail"
