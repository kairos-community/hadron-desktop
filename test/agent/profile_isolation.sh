#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
agent_root="$repo_root/rootfs-agent"
dockerfile="$repo_root/Dockerfile.agent"

assert_file() {
  local path="$1"
  if [ ! -f "$path" ]; then
    echo "missing expected file: $path" >&2
    exit 1
  fi
}

assert_contains() {
  local needle="$1"
  local file="$2"
  if ! grep -F -- "$needle" "$file" >/dev/null; then
    echo "missing expected content in $file: $needle" >&2
    exit 1
  fi
}

assert_not_contains() {
  local needle="$1"
  local file="$2"
  if grep -F -- "$needle" "$file" >/dev/null; then
    echo "unexpected content in $file: $needle" >&2
    exit 1
  fi
}

assert_file "$agent_root/etc/hadron-agent/profile"
assert_file "$agent_root/etc/hadron-desktop/session"
assert_contains 'DESKTOP_NAME=i3' "$agent_root/etc/hadron-desktop/session"
assert_contains 'DISPLAY_NAME=XLibre' "$agent_root/etc/hadron-desktop/session"
assert_contains 'PROFILE=agent' "$agent_root/etc/hadron-agent/profile"

assert_contains 'COPY rootfs-agent/ /' "$dockerfile"
assert_contains 'COPY --from=cua-build /cua/ /' "$dockerfile"
