#!/usr/bin/env bash
# Tests for hadron-agent-browser-install: installs org.chromium.Chromium
# system-wide on first boot, idempotently, through the HADRON_FLATPAK seam.
#
# The real flatpak is intercepted by a fake that appends each invocation to
# $CALLS and answers `info` from a control file, so a test can assert exactly
# which flatpak subcommands ran without pulling ~2 GB or hitting the network.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/rootfs-agent/usr/bin/hadron-agent-browser-install"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# 1. syntax
if sh -n "$script" 2>/dev/null; then ok "sh -n"; else err "sh -n failed"; fi

# Build a fake flatpak: `info --system org.chromium.Chromium` exits 0 iff the
# control file $INSTALLED_FLAG exists; everything else records its args and, for
# `install`, creates the flag (so a later `info` reports it present).
make_fake_flatpak() { # $1 = dir to hold the fake + its logs
  d="$1"
  cat > "$d/flatpak" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$d/calls"
case "\$1 \$2" in
  "info --system")
    shift 2
    [ "\$1" = "org.chromium.Chromium" ] && { [ -e "$d/installed" ] && exit 0 || exit 1; } ;;
  "install --system"|"install --noninteractive")
    if [ -e "$d/fail_install" ]; then exit 1; fi
    : > "$d/installed" ;;
esac
exit 0
EOF
  chmod +x "$d/flatpak"
}

# 2. already installed -> no remote-add, no install
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/installed"
HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
  sh "$script" >/dev/null 2>&1
if grep -q "install" "$d/calls" 2>/dev/null; then
  err "already-installed run must not call install"
else ok "already installed -> no install"; fi
rm -rf "$d"

# 3. not installed -> adds the remote then installs
d="$(mktemp -d)"; make_fake_flatpak "$d"
HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
  sh "$script" >/dev/null 2>&1
if grep -q "remote-add" "$d/calls" && grep -q "install" "$d/calls"; then
  ok "absent -> remote-add + install"
else err "absent run must add the remote and install (got: $(cat "$d/calls"))"; fi
rm -rf "$d"

# 4. install failure -> script exits non-zero (unit will retry next boot)
d="$(mktemp -d)"; make_fake_flatpak "$d"; : > "$d/fail_install"
if HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER=/dev/null \
     sh "$script" >/dev/null 2>&1; then
  err "a failed install must make the script exit non-zero"
else ok "install failure -> non-zero exit"; fi
rm -rf "$d"

# 5. marker absent -> clean no-op: exit 0 and flatpak never invoked at all.
# Guards the defence that stops a manual run on a non-appliance from pulling 2 GB.
d="$(mktemp -d)"; make_fake_flatpak "$d"
if HADRON_FLATPAK="$d/flatpak" HADRON_PROFILE_MARKER="$d/nonexistent-marker" \
     sh "$script" >/dev/null 2>&1; then
  if [ -e "$d/calls" ]; then
    err "marker absent must not invoke flatpak at all (got: $(cat "$d/calls"))"
  else ok "marker absent -> exit 0, flatpak never called"; fi
else err "marker absent must exit 0 (clean no-op)"; fi
rm -rf "$d"

exit "$fail"
