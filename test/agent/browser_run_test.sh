#!/usr/bin/env bash
# Tests for hadron-agent-browser-run: the launch wrapper the user unit execs.
# A fake flatpak records the `run` argv and answers `info` from a control file,
# so we assert the launch flags without starting a real browser.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/rootfs-agent/usr/bin/hadron-agent-browser-run"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

if sh -n "$script" 2>/dev/null; then ok "sh -n"; else err "sh -n failed"; fi

make_fake_flatpak() { # $1 = dir; presence of $1/installed => info exits 0
  d="$1"
  cat > "$d/flatpak" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$d/calls"
case "\$1 \$2" in
  "info --system") [ -e "$d/installed" ] && exit 0 || exit 1 ;;
  "run "*) : > "$d/ran" ;;    # would exec; record and return so the test continues
esac
exit 0
EOF
  chmod +x "$d/flatpak"
}

# 1. app not installed -> does not run, exits non-zero (unit retries)
d="$(mktemp -d)"; make_fake_flatpak "$d"
if DISPLAY=:0 HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
     sh "$script" >/dev/null 2>&1; then
  err "must not launch before the app is installed"
else ok "app absent -> non-zero, no launch"; fi
[ -e "$d/ran" ] && err "launched despite app absent"
rm -rf "$d"

# 2. no DISPLAY -> does not run, exits non-zero
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
if env -u DISPLAY HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
     sh "$script" >/dev/null 2>&1; then
  err "must not launch with no DISPLAY"
else ok "no DISPLAY -> non-zero, no launch"; fi
rm -rf "$d"

# 3. installed + DISPLAY -> execs flatpak run with the CDP port and hardening flags
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
DISPLAY=:0 HADRON_FLATPAK="$d/flatpak" HADRON_BROWSER_EXEC=1 \
  sh "$script" >/dev/null 2>&1
calls="$(cat "$d/calls" 2>/dev/null)"
for needle in "run" "org.chromium.Chromium" "--remote-debugging-port=9222" \
              "--no-sandbox" "--ozone-platform=x11"; do
  case "$calls" in *"$needle"*) : ;; *) err "launch missing $needle (got: $calls)";; esac
done
[ -e "$d/ran" ] && ok "installed + DISPLAY -> flatpak run with CDP port"
rm -rf "$d"

exit "$fail"
