#!/usr/bin/env bash
# Run all boot-UX checks against a built image.
#
#   test/boot-ux/run.sh [IMAGE] [SUBTITLE]
#
# Stages the ISO overlay (without building an ISO) so the overlay checks can run
# cheaply. Does not boot a VM — see the plan's manual verification steps for that.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

IMAGE="${1:-sway-desktop:dev}"
SUBTITLE="${2:-sway · wayland · kairos}"
OVERLAY_PARENT="build/boot-ux"
fail=0

"$SCRIPT_DIR/check-splash.sh"    "$IMAGE" || fail=1
"$SCRIPT_DIR/check-initramfs.sh" "$IMAGE" || fail=1
"$SCRIPT_DIR/check-branding.sh"  "$IMAGE" "$SUBTITLE" || fail=1

STAGE_ONLY=1 auroraboot/build.sh "$IMAGE" "$OVERLAY_PARENT" >/dev/null
"$SCRIPT_DIR/check-iso-overlay.sh" "$OVERLAY_PARENT/iso-overlay" "$SUBTITLE" || fail=1

if [ "$fail" -eq 0 ]; then echo "boot-ux: ALL PASS"; else echo "boot-ux: FAILURES"; fi
exit "$fail"
