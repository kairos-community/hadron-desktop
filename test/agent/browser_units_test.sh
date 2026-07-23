#!/usr/bin/env bash
# Assert the Chromium install/launch units are present and enabled in the built
# agent image, and that the signed Flathub descriptor they depend on is shipped.
#
#   test/agent/browser_units_test.sh [IMAGE]
#
# This is an IMAGE check (docker-run assertions against the built agent image),
# the sibling of test/boot-ux/check-*.sh. Unlike browser_{install,persistence,
# run}_test.sh -- which exercise the scripts/stages statically with no image --
# this one proves the overlay actually lands the units in agent-desktop:dev and
# that the committed multi-user.target.wants symlink (the ONLY enablement
# mechanism, since `COPY rootfs-agent/ /` never processes a unit's [Install]
# section) survives into the built image.
set -euo pipefail
IMAGE="${1:-agent-desktop:dev}"
fail=0
check() {
  local desc="$1"; shift
  if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
    echo "PASS: $desc"
  else echo "FAIL: $desc"; fail=1; fi
}

check "install script present and executable" 'test -x /usr/bin/hadron-agent-browser-install'
check "launch wrapper present and executable" 'test -x /usr/bin/hadron-agent-browser-run'
check "install unit present"        'test -f /etc/systemd/system/hadron-agent-browser-install.service'
check "install unit enabled (multi-user.target.wants)" \
  'test -L /etc/systemd/system/multi-user.target.wants/hadron-agent-browser-install.service'
check "launch user unit present"    'test -f /usr/lib/systemd/user/hadron-agent-browser.service'
check "signed Flathub descriptor shipped" 'test -f /usr/share/hadron/flathub.flatpakrepo'
check "flatpak available in the image" 'command -v flatpak'
check "session-ready starts the browser unit" \
  'grep -q "systemctl --user start hadron-agent-browser.service" /usr/bin/hadron-agent-session-ready'

exit "$fail"
