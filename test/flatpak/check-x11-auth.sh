#!/bin/sh
# Checks that the i3 session launcher gives Flatpak apps the X cookie: it runs
# `flatpak override --user` with the file named by XAUTHORITY, read-only, and
# sets XAUTHORITY to the same path inside the sandbox. A fake flatpak and a fake
# i3 stand in for the real ones.
#
#   test/flatpak/check-x11-auth.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
launcher="$here/../../rootfs-i3/usr/bin/start-desktop"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"

printf '#!/bin/sh\necho "$@" >> "%s/flatpak.log"\n' "$work" > "$work/bin/flatpak"
printf '#!/bin/sh\nexit 0\n' > "$work/bin/i3"
chmod +x "$work/bin/flatpak" "$work/bin/i3"

fail=0
# DBUS_SESSION_BUS_ADDRESS is set so the launcher goes straight to i3.
PATH="$work/bin:$PATH" XAUTHORITY=/run/user/1234/lyxauth DBUS_SESSION_BUS_ADDRESS=unix:path=/x \
  sh "$launcher"
expect="override --user --filesystem=/run/user/1234/lyxauth:ro --env=XAUTHORITY=/run/user/1234/lyxauth"
if [ "$(cat "$work/flatpak.log")" = "$expect" ]; then
  echo "PASS: the cookie file is passed to Flatpak"
else
  echo "FAIL: flatpak was called with: $(cat "$work/flatpak.log" 2>/dev/null)"; fail=1
fi

rm -f "$work/flatpak.log"
PATH="$work/bin:$PATH" DBUS_SESSION_BUS_ADDRESS=unix:path=/x sh -c 'unset XAUTHORITY; exec sh "$0"' "$launcher"
if [ ! -e "$work/flatpak.log" ]; then
  echo "PASS: no override without XAUTHORITY"
else
  echo "FAIL: override ran without XAUTHORITY"; fail=1
fi
exit "$fail"
