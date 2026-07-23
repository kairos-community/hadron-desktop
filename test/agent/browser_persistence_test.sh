#!/usr/bin/env bash
# Tests for 94_agent_browser_persistence.yaml: appends /var/lib/flatpak to
# CUSTOM_BIND_MOUNTS without clobbering the entries 92_docker and 93_agent add,
# and is idempotent across re-runs of cos-setup.
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
stage="$repo_root/rootfs-agent/system/oem/94_agent_browser_persistence.yaml"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# Pull the shell body out of the yip stage's `commands: - |` block. The block is
# indented 10 spaces (name/commands/-| nesting); strip that indent and run it.
extract_cmd() {
  awk '
    /commands:/ {inc=1; next}
    inc && /^[[:space:]]*- \|/ {body=1; next}
    body {
      if ($0 !~ /^[[:space:]]{10}/ && $0 !~ /^[[:space:]]*$/) {body=0; next}
      sub(/^[[:space:]]{10}/, ""); print
    }
  ' "$stage"
}

run_stage() { # $1 = layout file the stage should edit
  # The stage body hardcodes `layout=/run/cos/cos-layout.env` (byte-identical to
  # 93_agent_persistence.yaml). Repoint just that assignment at the test's fake
  # layout so we exercise the real read-modify-write against a temp file instead
  # of the live system path. All logic under test is left untouched.
  HOME=/dev/null sh -c "$(extract_cmd | sed "s#^layout=/run/cos/cos-layout.env\$#layout='$1'#")"
}

# Helper: what CUSTOM_BIND_MOUNTS ends up as.
value_of() { sed -n 's/^CUSTOM_BIND_MOUNTS="\(.*\)"$/\1/p' "$1"; }

# 1. appends alongside existing docker + hadron-agent entries
w="$(mktemp)"; printf 'CUSTOM_BIND_MOUNTS="/var/lib/docker /var/lib/hadron-agent"\n' > "$w"
run_stage "$w"
v="$(value_of "$w")"
case " $v " in
  *" /var/lib/docker "*) : ;; *) err "dropped /var/lib/docker (got: $v)";; esac
case " $v " in
  *" /var/lib/hadron-agent "*) : ;; *) err "dropped /var/lib/hadron-agent (got: $v)";; esac
case " $v " in
  *" /var/lib/flatpak "*) ok "appended /var/lib/flatpak, kept the rest";;
  *) err "did not append /var/lib/flatpak (got: $v)";; esac
rm -f "$w"

# 2. idempotent: a second run does not duplicate the entry
w="$(mktemp)"; printf 'CUSTOM_BIND_MOUNTS="/var/lib/docker"\n' > "$w"
run_stage "$w"; run_stage "$w"
count="$(value_of "$w" | tr ' ' '\n' | grep -cx '/var/lib/flatpak')"
if [ "$count" -eq 1 ]; then ok "idempotent (one /var/lib/flatpak entry)"; \
  else err "duplicated /var/lib/flatpak ($count times)"; fi
rm -f "$w"

# 3. empty/unset layout still yields a valid single entry
w="$(mktemp)"; : > "$w"
run_stage "$w"
if [ "$(value_of "$w")" = "/var/lib/flatpak" ]; then ok "empty layout -> single entry"; \
  else err "empty layout produced: $(value_of "$w")"; fi
rm -f "$w"

exit "$fail"
