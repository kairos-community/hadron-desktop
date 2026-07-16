#!/usr/bin/env bash
# test/agent/make-seed.sh -- mint per-VM bearer credentials and build a
# Kairos `cidata` (NoCloud) seed ISO for the Cua agent E2E fixture
# (Phase 4, Task 2).
#
# WHAT THIS PRODUCES (all under <seed-dir>; every file that ever holds
# plaintext, or the seed dir itself, is mode 0600/0700):
#   user-data          -- #cloud-config text consumed by the Phase-3
#                          provisioner (see the parsing contract below).
#                          Contains ONLY digests, never bearer plaintext.
#   meta-data           -- empty file; Kairos NoCloud requires it to exist.
#   <iso>               -- ISO9660 volume labeled "cidata" (default:
#                          <seed-dir>/seed.iso), built with the exact
#                          xorriso invocation Kairos's own NoCloud tests use.
#   user.token          -- plaintext "hdn_u_..." bearer (0600). Always minted.
#   admin.token         -- plaintext "hdn_a_..." bearer (0600). Minted unless
#                          --no-admin is given.
#
# CLOUD-CONFIG SHAPE (must match what the Phase-3 provisioner parses; see
# agent/internal/auth/token.go for the bearer/digest format this mirrors):
#   Top-level `install:` (auto/device/reboot) is present ONLY in
#   --mode=install (the default, a full zero-touch install seed) and is
#   OMITTED ENTIRELY in --mode=live (a live/contract seed that boots an
#   already-installed disk, where install: would be a no-op at best).
#   Top-level `hadron_agent:` is always present. `auth.admin_token_hash` is
#   OMITTED (not left blank) when --no-admin is given, and no admin bearer
#   is minted for that seed.
#
# SECRET HYGIENE:
#   - Bearer plaintext is written ONLY to the two 0600 token files above.
#   - A bearer is NEVER passed as an argv element to any *external* command
#     (so it can never show up in `ps`); it only ever flows through
#     variables, builtins (printf), and pipes.
#   - Nothing this script prints (stdout or stderr) ever contains bearer
#     plaintext -- only digests (sha256:<hex>) and absolute file paths --
#     and the final summary is still routed through common.sh's
#     hdn_agent_redact as defense in depth.
#
# STDOUT CONTRACT (KEY=VALUE lines, one per line, safe to log/capture;
# consumed by later fixture tooling):
#   SEED_DIR=<absolute path>
#   USER_DATA=<absolute path to user-data>
#   META_DATA=<absolute path to meta-data>
#   SEED_ISO=<absolute path to the built ISO>
#   USER_TOKEN_FILE=<absolute path>
#   USER_TOKEN_DIGEST=sha256:<64 lowercase hex>
#   ADMIN_TOKEN_FILE=<absolute path>             # only when admin was minted
#   ADMIN_TOKEN_DIGEST=sha256:<64 lowercase hex>  # only when admin was minted
#
# Usage:
#   test/agent/make-seed.sh <seed-dir> [options]
#
# Options:
#   --mode install|live   install: full zero-touch install seed (default).
#                          live: live/contract seed, omits the install: block.
#   --live / --install     shorthand for --mode live / --mode install.
#   --no-admin              mint no admin bearer; omit auth.admin_token_hash.
#   --hostname NAME         cloud-config hostname (default: hadron-agent-e2e)
#   --device PATH            install.device (default: /dev/vda; ignored
#                          under --mode=live)
#   --listen ADDR           hadron_agent.endpoint.listen (default:
#                          0.0.0.0:7443)
#   --iso PATH               seed ISO output path (default:
#                          <seed-dir>/seed.iso)
#
# Requires: openssl, xorriso, and sha256sum or shasum. No other build deps.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

err() { echo "make-seed: $*" >&2; }
die() { err "$*"; exit 1; }

usage() {
  cat >&2 <<'USAGE'
usage: make-seed.sh <seed-dir> [--mode install|live] [--live] [--install]
                     [--no-admin] [--hostname NAME] [--device PATH]
                     [--listen ADDR] [--iso PATH]
USAGE
}

MODE="install"
ADMIN=1
HOSTNAME_VAL="hadron-agent-e2e"
DEVICE="/dev/vda"
LISTEN="0.0.0.0:7443"
SEED_ISO=""
SEED_DIR=""

while [ $# -gt 0 ]; do
  case "$1" in
    --mode)
      [ $# -ge 2 ] || { usage; die "--mode requires a value"; }
      MODE="$2"; shift 2 ;;
    --mode=*) MODE="${1#*=}"; shift ;;
    --live) MODE="live"; shift ;;
    --install) MODE="install"; shift ;;
    --no-admin) ADMIN=0; shift ;;
    --hostname)
      [ $# -ge 2 ] || { usage; die "--hostname requires a value"; }
      HOSTNAME_VAL="$2"; shift 2 ;;
    --hostname=*) HOSTNAME_VAL="${1#*=}"; shift ;;
    --device)
      [ $# -ge 2 ] || { usage; die "--device requires a value"; }
      DEVICE="$2"; shift 2 ;;
    --device=*) DEVICE="${1#*=}"; shift ;;
    --listen)
      [ $# -ge 2 ] || { usage; die "--listen requires a value"; }
      LISTEN="$2"; shift 2 ;;
    --listen=*) LISTEN="${1#*=}"; shift ;;
    --iso)
      [ $# -ge 2 ] || { usage; die "--iso requires a value"; }
      SEED_ISO="$2"; shift 2 ;;
    --iso=*) SEED_ISO="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) usage; die "unknown option: $1" ;;
    *)
      [ -z "$SEED_DIR" ] || { usage; die "unexpected extra argument: $1"; }
      SEED_DIR="$1"; shift ;;
  esac
done

[ -n "$SEED_DIR" ] || { usage; die "seed-dir is required"; }
case "$MODE" in
  install|live) ;;
  *) die "invalid --mode '$MODE' (expected install or live)" ;;
esac

for bin in openssl xorriso; do
  command -v "$bin" >/dev/null 2>&1 || die "required command not found: $bin"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  die "required command not found: sha256sum or shasum"
fi

mkdir -p "$SEED_DIR"
chmod 700 "$SEED_DIR"
SEED_DIR="$(cd "$SEED_DIR" && pwd)"

[ -n "$SEED_ISO" ] || SEED_ISO="$SEED_DIR/seed.iso"
case "$SEED_ISO" in
  /*) ;;
  *) SEED_ISO="$PWD/$SEED_ISO" ;;
esac

USER_DATA="$SEED_DIR/user-data"
META_DATA="$SEED_DIR/meta-data"
USER_TOKEN_FILE="$SEED_DIR/user.token"
ADMIN_TOKEN_FILE="$SEED_DIR/admin.token"

# --- hashing: complete-bearer-string SHA-256, lowercase hex ----------------
hash_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

# --- bearer generation: 32 crypto-random bytes, base64url, unpadded --------
# Mirrors agent/internal/auth.Generate exactly: prefix + RawURLEncoding of
# 32 random bytes (43 characters, no padding). openssl's *standard* base64
# of exactly 32 bytes always emits one trailing '=' (32 % 3 == 2), so
# translating the alphabet and stripping that one padding char is
# byte-for-byte equivalent to unpadded base64url.
gen_bearer() {
  local prefix="$1" body
  body="$(openssl rand 32 | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
  printf '%s%s' "$prefix" "$body"
}

write_secret() {
  # write_secret <path> <content> -- writes content (no trailing newline)
  # to a fresh 0600 file. The secret is passed as a shell variable/builtin
  # argument only, never to an external command's argv.
  local path="$1" content="$2"
  : > "$path"
  chmod 600 "$path"
  printf '%s' "$content" > "$path"
}

USER_BEARER="$(gen_bearer hdn_u_)"
USER_DIGEST="$(printf '%s' "$USER_BEARER" | hash_stdin)"
write_secret "$USER_TOKEN_FILE" "$USER_BEARER"
unset USER_BEARER

ADMIN_DIGEST=""
if [ "$ADMIN" -eq 1 ]; then
  ADMIN_BEARER="$(gen_bearer hdn_a_)"
  ADMIN_DIGEST="$(printf '%s' "$ADMIN_BEARER" | hash_stdin)"
  write_secret "$ADMIN_TOKEN_FILE" "$ADMIN_BEARER"
  unset ADMIN_BEARER
fi

# --- user-data: digests only, install: block only in --mode=install --------
{
  echo "#cloud-config"
  echo "hostname: ${HOSTNAME_VAL}"
  if [ "$MODE" = "install" ]; then
    echo "install:"
    echo "  auto: true"
    echo "  device: ${DEVICE}"
    echo "  reboot: true"
  fi
  echo "hadron_agent:"
  echo "  enabled: true"
  echo "  endpoint:"
  echo "    listen: \"${LISTEN}\""
  echo "    mdns: false"
  echo "  auth:"
  echo "    user_token_hash: \"sha256:${USER_DIGEST}\""
  if [ "$ADMIN" -eq 1 ]; then
    echo "    admin_token_hash: \"sha256:${ADMIN_DIGEST}\""
  fi
} > "$USER_DATA"
chmod 600 "$USER_DATA"

: > "$META_DATA"
chmod 600 "$META_DATA"

# --- ISO: exact invocation Kairos's own NoCloud tests use -------------------
rm -f "$SEED_ISO"
xorriso -as mkisofs -quiet -V cidata \
  -graft-points -o "$SEED_ISO" \
  user-data="$USER_DATA" meta-data="$META_DATA" \
  >/dev/null
chmod 600 "$SEED_ISO"

# --- summary: paths + digests only, never bearer plaintext -----------------
{
  echo "SEED_DIR=${SEED_DIR}"
  echo "USER_DATA=${USER_DATA}"
  echo "META_DATA=${META_DATA}"
  echo "SEED_ISO=${SEED_ISO}"
  echo "USER_TOKEN_FILE=${USER_TOKEN_FILE}"
  echo "USER_TOKEN_DIGEST=sha256:${USER_DIGEST}"
  if [ "$ADMIN" -eq 1 ]; then
    echo "ADMIN_TOKEN_FILE=${ADMIN_TOKEN_FILE}"
    echo "ADMIN_TOKEN_DIGEST=sha256:${ADMIN_DIGEST}"
  fi
} | hdn_agent_redact
