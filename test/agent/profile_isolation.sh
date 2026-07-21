#!/usr/bin/env bash
# Profile-isolation proof for the opt-in agent overlay (Phase 3, Task 9).
#
# This is NOT a Dockerfile/source-tree grep. It builds (or reuses) the three
# real images, `docker export`s each one's actual filesystem, and asserts
# against that exported content:
#
#   1. The ordinary Sway AND i3 images contain NONE of the agent-only paths
#      (hadron-agent binary, cua-driver, /etc/hadron-agent, the
#      hadron-agent-*.service units, the agent Ly drop-in) and have no
#      agent-only accounts in /etc/passwd.
#   2. The agent image DOES contain every one of those paths, its base
#      session marker says i3/XLibre, and it has the hadron-agent-gateway
#      system account but NOT the human `agent` account (that one is created
#      at boot, not baked into the image -- see Dockerfile.agent).
#   3. The agent image's file CONTENTS (not just names) carry no live
#      hdn_u_/hdn_a_ bearer, no configured (non-placeholder) bearer digest,
#      and no PEM private-key block -- proving the generic ISO embeds no
#      runtime secret. Tokens/keys are seed data written at boot by the OEM
#      provisioning stage, never Docker build args (see README).
#
# Usage:
#   bash test/agent/profile_isolation.sh
#   SKIP_BUILD=1 bash test/agent/profile_isolation.sh   # reuse existing images
#
# Exit code 0 = every assertion passed. Non-zero otherwise.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root" || exit 1

SWAY_IMAGE="sway-desktop:dev"
I3_IMAGE="i3-desktop:dev"
AGENT_IMAGE="agent-desktop:dev"

fail=0
err() { echo -e "\033[1;31mFAIL:\033[0m $*" >&2; fail=1; }
ok()  { echo -e "ok: $*"; }
log() { echo -e "\n\033[1;34m[isolation]\033[0m $*"; }

# ---------------------------------------------------------------------------
# scratch space + cleanup
# ---------------------------------------------------------------------------
work="$(mktemp -d "${TMPDIR:-/tmp}/agent-isolation.XXXXXX")"
containers=()

cleanup() {
  local cid
  for cid in "${containers[@]:-}"; do
    [ -n "$cid" ] && docker rm -f "$cid" >/dev/null 2>&1 || true
  done
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 1. Build (or reuse) the three images
# ---------------------------------------------------------------------------
if [ "${SKIP_BUILD:-0}" != "1" ]; then
  log "Building $SWAY_IMAGE (make DESKTOP=sway image)"
  make DESKTOP=sway image || { err "sway image build failed"; exit 1; }

  log "Building $I3_IMAGE (make DESKTOP=i3 image)"
  make DESKTOP=i3 image || { err "i3 image build failed"; exit 1; }

  log "Building $AGENT_IMAGE (make agent-image)"
  make agent-image || { err "agent image build failed"; exit 1; }
else
  log "SKIP_BUILD=1: reusing existing images (must already exist)"
  for img in "$SWAY_IMAGE" "$I3_IMAGE" "$AGENT_IMAGE"; do
    docker image inspect "$img" >/dev/null 2>&1 \
      || { err "SKIP_BUILD=1 but $img does not exist locally"; exit 1; }
  done
fi

# ---------------------------------------------------------------------------
# helpers: export an image's real filesystem to a tarball + flat file list
# ---------------------------------------------------------------------------

# export_image <image> <name> -> sets <name>_TAR and <name>_LIST globals
export_image() {
  local image="$1" name="$2" cid tarfile listfile
  log "Exporting $image filesystem (docker create + docker export)"
  cid="$(docker create "$image")" || { err "docker create $image failed"; exit 1; }
  containers+=("$cid")
  tarfile="$work/$name.tar"
  listfile="$work/$name.list"
  docker export "$cid" -o "$tarfile" || { err "docker export $image failed"; exit 1; }
  tar -tf "$tarfile" > "$listfile"
  docker rm -f "$cid" >/dev/null 2>&1 || true
  printf -v "${name}_TAR" '%s' "$tarfile"
  printf -v "${name}_LIST" '%s' "$listfile"
}

# in_list <listfile> <path...> -> true if ANY path is present (as file or dir)
in_list() {
  local listfile="$1"; shift
  local p
  for p in "$@"; do
    grep -qE "^${p}\$|^${p}/\$" "$listfile" && return 0
  done
  return 1
}

# extract_one <tarfile> <path> -> stdout the entry's content (empty if absent)
extract_one() {
  tar -xOf "$1" "$2" 2>/dev/null
}

# ---------------------------------------------------------------------------
# secret-scan regexes, factored into functions so BOTH the real docker-export
# scan (section 4 below) and the hermetic fixture self-test (section 5) run
# the exact same logic -- no risk of the two silently diverging.
# ---------------------------------------------------------------------------

# scan_bearer <dir> -> stdout: any live hdn_u_/hdn_a_ bearer found under <dir>.
# Prefix + exactly the 43-char unpadded base64url payload (32 random bytes)
# that auth.Generate produces -- see agent/internal/auth/token.go.
scan_bearer() {
  grep -rIoE 'hdn_[ua]_[A-Za-z0-9_-]{43}' "$1" 2>/dev/null || true
}

# scan_digest <dir> -> stdout: any configured (non-placeholder) bearer
# digest. The only digest the generic image may legitimately contain is
# auth's all-zero "unusable" placeholder (cmd/hadron-agent/main.go); any
# OTHER sha256:<64 hex> is a real configured credential.
scan_digest() {
  grep -rIoE 'sha256:[0-9a-f]{64}' "$1" 2>/dev/null | grep -vE ':sha256:0{64}$' || true
}

# scan_pem <dir> -> stdout: paths of files containing a PEM private-key
# block. Requires BOTH a BEGIN and a matching END marker in the SAME file
# (not just the word "PRIVATE KEY" appearing once -- e.g. the shared
# mime-type magic database legitimately lists the
# "-----BEGIN PGP PRIVATE KEY BLOCK-----" signature string for sniffing,
# with no matching END and no key body; that is not a leaked key).
scan_pem() {
  grep -rIlzP '(?s)-----BEGIN[ A-Z]*PRIVATE KEY-----.*?-----END[ A-Z]*PRIVATE KEY-----' "$1" 2>/dev/null | tr '\0' '\n' || true
}

export_image "$SWAY_IMAGE" SWAY
export_image "$I3_IMAGE" I3
export_image "$AGENT_IMAGE" AGENT

# Agent-only paths that must be entirely absent from the ordinary images and
# entirely present in the agent image. Directory entries in a `tar -t`
# listing carry a trailing slash; the units are individual files that may be
# copied straight into /etc/systemd/system OR symlinked into a *.wants/ dir,
# so match on basename via a list-wide grep rather than the full path.
AGENT_UNIT_BASENAMES=(
  hadron-agent-autoinstall.service
  hadron-agent-display-watchdog.service
  hadron-agent-gateway.service
  hadron-agent-provision.service
  hadron-agent-root-helper.service
)
AGENT_USER_UNIT_BASENAMES=(
  hadron-agent-session.service
)

# Additional agent-exclusive paths introduced by rootfs-agent/ that exist in
# NO other rootfs overlay (rootfs, rootfs-sway, rootfs-i3). Matched via
# in_list on the exact exported-tar path (as COPY rootfs-agent/ / in
# Dockerfile.agent places them 1:1 at the filesystem root). NOTE:
# usr/bin/hadron-status is deliberately NOT included here: rootfs-agent's
# copy is a CONTENT OVERRIDE of a path that already exists in rootfs-i3, not
# a novel path, so it would be a false positive in an absence check.
AGENT_ONLY_PATHS=(
  system/oem/90_agent_profile.yaml
  system/oem/91_agent_installer.yaml
  etc/i3/config.d/90-hadron-agent.conf
  etc/tmpfiles.d/hadron-agent.conf
  usr/bin/hadron-agent-first-run
  usr/bin/hadron-agent-session-ready
  usr/local/bin/hadron-agent-install
)

# ---------------------------------------------------------------------------
# 2. Ordinary images (Sway, i3) must LACK every agent path
# ---------------------------------------------------------------------------
assert_ordinary_lacks_agent() {
  local label="$1" listfile="$2" tarfile="$3"

  if in_list "$listfile" 'usr/bin/hadron-agent'; then
    err "$label: unexpectedly contains usr/bin/hadron-agent"
  else
    ok "$label: no usr/bin/hadron-agent"
  fi

  if in_list "$listfile" 'usr/bin/cua-driver'; then
    err "$label: unexpectedly contains usr/bin/cua-driver"
  else
    ok "$label: no usr/bin/cua-driver"
  fi

  if in_list "$listfile" 'etc/hadron-agent' 'etc/hadron-agent/defaults.json' 'etc/hadron-agent/profile'; then
    err "$label: unexpectedly contains /etc/hadron-agent"
  else
    ok "$label: no /etc/hadron-agent"
  fi

  local u hit=0
  for u in "${AGENT_UNIT_BASENAMES[@]}" "${AGENT_USER_UNIT_BASENAMES[@]}"; do
    if grep -q "$u\$" "$listfile"; then
      err "$label: unexpectedly ships rootfs-agent unit $u"
      hit=1
    fi
  done
  [ "$hit" -eq 0 ] && ok "$label: no hadron-agent-*.service units"

  local p hit2=0
  for p in "${AGENT_ONLY_PATHS[@]}"; do
    if in_list "$listfile" "$p"; then
      err "$label: unexpectedly contains $p"
      hit2=1
    fi
  done
  [ "$hit2" -eq 0 ] && ok "$label: no agent-exclusive OEM/i3/tmpfiles/binary paths"

  if in_list "$listfile" 'etc/systemd/system/ly@.service.d/20-agent-profile.conf'; then
    err "$label: unexpectedly contains the agent Ly override 20-agent-profile.conf"
  else
    ok "$label: no agent Ly override (ly@.service.d/20-agent-profile.conf)"
  fi

  # Belt-and-suspenders: neither variant creates a per-instance Ly override
  # directory at all (only the shared ly@.service.d template dir exists).
  if grep -q 'ly@tty1\.service\.d' "$listfile"; then
    err "$label: unexpectedly contains a ly@tty1.service.d override"
  else
    ok "$label: no ly@tty1.service.d override"
  fi

  # No agent accounts baked into /etc/passwd either.
  local passwd
  passwd="$(extract_one "$tarfile" etc/passwd)"
  if printf '%s\n' "$passwd" | grep -qE '^(agent|hadron-agent-gateway):'; then
    err "$label: /etc/passwd unexpectedly has an agent/hadron-agent-gateway account"
  else
    ok "$label: /etc/passwd has no agent/hadron-agent-gateway account"
  fi
}

log "Asserting Sway image lacks all agent paths/accounts"
assert_ordinary_lacks_agent sway "$SWAY_LIST" "$SWAY_TAR"

log "Asserting i3 image lacks all agent paths/accounts"
assert_ordinary_lacks_agent i3 "$I3_LIST" "$I3_TAR"

# ---------------------------------------------------------------------------
# 3. Agent image must CONTAIN every agent path + the right session marker
# ---------------------------------------------------------------------------
log "Asserting agent image contains all expected agent paths"

if in_list "$AGENT_LIST" 'usr/bin/hadron-agent'; then
  ok "agent: has usr/bin/hadron-agent"
else
  err "agent: missing usr/bin/hadron-agent"
fi

if in_list "$AGENT_LIST" 'usr/bin/cua-driver'; then
  ok "agent: has usr/bin/cua-driver"
else
  err "agent: missing usr/bin/cua-driver"
fi

if in_list "$AGENT_LIST" 'etc/hadron-agent/defaults.json'; then
  ok "agent: has /etc/hadron-agent/defaults.json"
else
  err "agent: missing /etc/hadron-agent/defaults.json"
fi

if in_list "$AGENT_LIST" 'etc/hadron-agent/profile'; then
  ok "agent: has /etc/hadron-agent/profile"
else
  err "agent: missing /etc/hadron-agent/profile"
fi

# Content, not just presence: rootfs-agent/etc/hadron-agent/profile ships
# `PROFILE=agent` (plus ENABLED=true) -- assert the exported file actually
# carries that marker, not merely that a path with this name exists.
profile_content="$(extract_one "$AGENT_TAR" etc/hadron-agent/profile)"
if printf '%s\n' "$profile_content" | grep -q '^PROFILE=agent$'; then
  ok "agent: /etc/hadron-agent/profile content says PROFILE=agent"
else
  err "agent: /etc/hadron-agent/profile content does not say PROFILE=agent (got: $profile_content)"
fi

miss=0
for u in "${AGENT_UNIT_BASENAMES[@]}" "${AGENT_USER_UNIT_BASENAMES[@]}"; do
  grep -q "$u\$" "$AGENT_LIST" || { err "agent: missing rootfs-agent unit $u"; miss=1; }
done
[ "$miss" -eq 0 ] && ok "agent: has all hadron-agent-*.service units"

miss2=0
for p in "${AGENT_ONLY_PATHS[@]}"; do
  in_list "$AGENT_LIST" "$p" || { err "agent: missing $p"; miss2=1; }
done
[ "$miss2" -eq 0 ] && ok "agent: has all agent-exclusive OEM/i3/tmpfiles/binary paths"

if in_list "$AGENT_LIST" 'etc/systemd/system/ly@.service.d/20-agent-profile.conf'; then
  ok "agent: has the agent Ly override (ly@.service.d/20-agent-profile.conf)"
else
  err "agent: missing the agent Ly override (ly@.service.d/20-agent-profile.conf)"
fi

session="$(extract_one "$AGENT_TAR" etc/hadron-desktop/session)"
if printf '%s\n' "$session" | grep -q 'DESKTOP_NAME=i3' \
  && printf '%s\n' "$session" | grep -q 'DISPLAY_NAME=XLibre'; then
  ok "agent: base session marker says i3/XLibre"
else
  err "agent: /etc/hadron-desktop/session does not say i3/XLibre (got: $session)"
fi

# Accounts: hadron-agent-gateway (system, locked) baked in; the human `agent`
# account is intentionally NOT -- it is created by the OEM/install path at
# boot (Task 5), never baked into the image. Use /etc/passwd directly: the
# base image ships no `getent` binary (Task 4 carry-forward).
agent_passwd="$(extract_one "$AGENT_TAR" etc/passwd)"
if printf '%s\n' "$agent_passwd" | grep -qE '^hadron-agent-gateway:'; then
  ok "agent: /etc/passwd has hadron-agent-gateway"
else
  err "agent: /etc/passwd missing hadron-agent-gateway"
fi
if printf '%s\n' "$agent_passwd" | grep -qE '^agent:'; then
  err "agent: /etc/passwd unexpectedly has the human 'agent' account baked in (must be created at boot)"
else
  ok "agent: /etc/passwd has no baked-in 'agent' account (created at boot)"
fi

# ---------------------------------------------------------------------------
# 4. Secret scan: the generic agent filesystem embeds no runtime secret
# ---------------------------------------------------------------------------
log "Secret-scanning the generic agent image (content, not just names)"

secrets_dir="$work/agent-fs"
mkdir -p "$secrets_dir"
# Extract everything except device/special nodes (unprivileged tar can't
# recreate those, and they can never carry text secrets anyway).
tar -xf "$AGENT_TAR" -C "$secrets_dir" --exclude='dev/*' --exclude='proc/*' --exclude='sys/*' 2>/dev/null || true

# Sanity guard: a botched/partial extraction would make every scan below
# vacuously pass (nothing to grep). Confirm a known agent file actually
# landed on disk before trusting a clean scan result.
if [ ! -f "$secrets_dir/etc/hadron-agent/defaults.json" ]; then
  err "secret scan: extraction of $AGENT_TAR into $secrets_dir looks incomplete (defaults.json absent); aborting scan"
fi

# -I skips binary files (ELF/.so/etc). This is deliberate, not a loophole:
# every credential this build could leak (a bearer, a configured digest, a
# TLS key) only ever gets INTO the image as text -- JSON/YAML/ini config or
# an OEM cloud-config fragment -- never as a Go string constant compiled
# into hadron-agent/cua-driver. Scanning binaries verbatim only produces
# noise (dockerd embeds unrelated OCI empty-layer sha256 constants; the
# hadron-agent binary embeds its own all-zero "unusable" placeholder digest
# as source code). -I keeps the scan focused on the surface a real leak
# would actually appear on.

# 4a. A live bearer (see scan_bearer above).
bearer_hits="$(scan_bearer "$secrets_dir")"
if [ -n "$bearer_hits" ]; then
  err "agent image embeds a live hdn_u_/hdn_a_ bearer:"
  printf '%s\n' "$bearer_hits" >&2
else
  ok "agent image embeds no hdn_u_/hdn_a_ bearer"
fi

# 4b. A configured (real) bearer digest (see scan_digest above).
digest_hits="$(scan_digest "$secrets_dir")"
if [ -n "$digest_hits" ]; then
  err "agent image embeds a configured (non-placeholder) bearer digest:"
  printf '%s\n' "$digest_hits" >&2
else
  ok "agent image embeds no configured bearer digest (only the all-zero placeholder, if any)"
fi

# 4c. A PEM private-key block (see scan_pem above).
pem_hits="$(scan_pem "$secrets_dir")"
if [ -n "$pem_hits" ]; then
  err "agent image embeds a PEM private-key block in:"
  printf '%s\n' "$pem_hits" >&2
else
  ok "agent image embeds no PEM private-key block"
fi

# ---------------------------------------------------------------------------
# 5. Regression fixture: prove the scan_bearer/scan_digest/scan_pem regexes
# THEMSELVES still catch a real-shaped secret. Section 4 above only proves
# the real generic agent filesystem has no secrets in it -- that scan would
# pass just as "cleanly" if a future typo silently broke a regex to match
# nothing. This feeds synthetic fixtures to the SAME functions used in
# section 4 (no separate/divergent copy of the logic) and requires no
# docker/image access, so it runs unconditionally.
# ---------------------------------------------------------------------------
log "Self-test: secret-scan regexes still catch real-shaped secrets (fixture, hermetic)"

selftest_dir="$work/secretscan-selftest"

# 1. A real-shaped bearer must be CAUGHT by scan_bearer.
mkdir -p "$selftest_dir/bearer_pos"
printf 'HADRON_BEARER=hdn_u_GlWBixHL8fvQwK0m0_sCeLFMxeJRWp7DwCS3Dd7rhcQ\n' \
  > "$selftest_dir/bearer_pos/fixture.env"
if [ -n "$(scan_bearer "$selftest_dir/bearer_pos")" ]; then
  ok "self-test: scan_bearer catches a real-shaped hdn_u_<43-char> bearer"
else
  err "self-test: scan_bearer FAILED to catch a real-shaped hdn_u_<43-char> bearer -- regex regressed!"
fi

# 2. A real (non-zero) digest must be CAUGHT by scan_digest.
mkdir -p "$selftest_dir/digest_pos"
printf 'bearer_digest: sha256:dc691a72424bad001b67b4852275139a47a35689f4a99e9c9b7670cb89cb6c85\n' \
  > "$selftest_dir/digest_pos/fixture.yaml"
if [ -n "$(scan_digest "$selftest_dir/digest_pos")" ]; then
  ok "self-test: scan_digest catches a real (non-zero) sha256:<64 hex> digest"
else
  err "self-test: scan_digest FAILED to catch a real sha256 digest -- regex regressed!"
fi

# 3. The all-zeros placeholder digest must NOT be caught (it is the
# documented "unusable" default, not a leaked credential).
mkdir -p "$selftest_dir/digest_neg"
printf 'bearer_digest: sha256:%s\n' "$(printf '0%.0s' $(seq 1 64))" \
  > "$selftest_dir/digest_neg/fixture.yaml"
if [ -z "$(scan_digest "$selftest_dir/digest_neg")" ]; then
  ok "self-test: scan_digest correctly excludes the all-zeros placeholder digest"
else
  err "self-test: scan_digest unexpectedly flagged the all-zeros placeholder digest -- exclusion regressed!"
fi

# 4. A real PEM private-key block (BEGIN + matching END) must be CAUGHT.
mkdir -p "$selftest_dir/pem_pos"
{
  echo "-----BEGIN EC PRIVATE KEY-----"
  echo "MIGkAgEBBDBGarbageNotARealKeyBodyJustFixtureDataForTheSelfTest=="
  echo "-----END EC PRIVATE KEY-----"
} > "$selftest_dir/pem_pos/fixture.pem"
if [ -n "$(scan_pem "$selftest_dir/pem_pos")" ]; then
  ok "self-test: scan_pem catches a real BEGIN/END EC PRIVATE KEY block"
else
  err "self-test: scan_pem FAILED to catch a real PEM private-key block -- regex regressed!"
fi

# 5. A benign lookalike (BEGIN marker with no matching END, e.g. the kind of
# signature string a mime-type magic database legitimately ships) must NOT
# be caught.
mkdir -p "$selftest_dir/pem_neg"
printf -- '-----BEGIN PGP PRIVATE KEY BLOCK-----\n(magic-db signature string, no key body, no END marker)\n' \
  > "$selftest_dir/pem_neg/fixture.txt"
if [ -z "$(scan_pem "$selftest_dir/pem_neg")" ]; then
  ok "self-test: scan_pem correctly ignores a BEGIN-only lookalike with no END marker"
else
  err "self-test: scan_pem unexpectedly flagged a BEGIN-only lookalike -- false-positive regressed!"
fi

# ---------------------------------------------------------------------------
echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: profile isolation and secret scan"
else
  echo "FAILED: one or more isolation/secret-scan checks failed" >&2
fi
exit "$fail"
