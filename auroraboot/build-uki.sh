#!/usr/bin/env bash
# Build a Trusted Boot (UKI) installer ISO with AuroraBoot, signed with the
# Secure Boot keys in KEYS_DIR.
#
#   auroraboot/build-uki.sh <IMAGE> <OUT_DIR> <KEYS_DIR>
#
# IMAGE must be built with TRUSTED_BOOT=true (`make uki-image`). KEYS_DIR holds
# what `auroraboot genkey` writes (`make uki-keys`): db.key and db.pem sign the
# UKIs, tpm2-pcr-private.pem signs the PCR policy that the disk encryption key
# is sealed to, and PK.auth, KEK.auth and db.auth are the public keys that
# systemd-boot enrolls when the firmware is in Secure Boot setup mode.
#
# Every machine installed from this ISO only boots UKIs signed with these keys,
# and its encrypted partitions only unlock under a PCR policy signed with them.
# Keep the private keys: an upgrade image signed with other keys does not boot.
#
# Env:
#   AURORA_IMAGE        AuroraBoot image (default matches the Makefile pin)
#   SECURE_BOOT_ENROLL  systemd-boot secure-boot-enroll: off, manual, if-safe
#                       (AuroraBoot's default: enroll on a VM in setup mode)
#                       or force
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${1:?usage: build-uki.sh <IMAGE> <OUT_DIR> <KEYS_DIR>}"
OUT_DIR="${2:?usage: build-uki.sh <IMAGE> <OUT_DIR> <KEYS_DIR>}"
KEYS_DIR="${3:?usage: build-uki.sh <IMAGE> <OUT_DIR> <KEYS_DIR>}"
AURORA_IMAGE="${AURORA_IMAGE:-quay.io/kairos/auroraboot:v0.29.0}"
SECURE_BOOT_ENROLL="${SECURE_BOOT_ENROLL:-if-safe}"

for f in db.key db.pem tpm2-pcr-private.pem PK.auth KEK.auth db.auth; do
  [ -s "$KEYS_DIR/$f" ] || {
    echo "build-uki: $KEYS_DIR/$f is missing; run 'make uki-keys' or point KEYS_DIR at your keys" >&2
    exit 1
  }
done

# A GRUB image has no business in a UKI: it would boot, but without the
# measured boot that Trusted Boot is for. kairos-init records the mode.
tb="$(docker run --rm --entrypoint sh "$IMAGE" -c \
  '. /etc/kairos-release && printf %s "$KAIROS_TRUSTED_BOOT"' 2>/dev/null || true)"
[ "$tb" = true ] || {
  echo "build-uki: $IMAGE was not built with TRUSTED_BOOT=true (KAIROS_TRUSTED_BOOT='$tb')" >&2
  exit 1
}

mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/*-uki.iso
OUT_ABS="$(cd "$OUT_DIR" && pwd)"
KEYS_ABS="$(cd "$KEYS_DIR" && pwd)"

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$OUT_ABS":/output \
  -v "$KEYS_ABS":/keys:ro \
  "$AURORA_IMAGE" build-uki \
  --output-dir /output \
  --output-type iso \
  --public-keys /keys \
  --tpm-pcr-private-key /keys/tpm2-pcr-private.pem \
  --sb-key /keys/db.key \
  --sb-cert /keys/db.pem \
  --secure-boot-enroll "$SECURE_BOOT_ENROLL" \
  --sdboot-in-source \
  --boot-branding "Hadron Desktop" \
  "docker:$IMAGE"

ls -l "$OUT_ABS"/*-uki.iso
