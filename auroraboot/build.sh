#!/usr/bin/env bash
# Build a branded installer ISO with AuroraBoot.
#
#   auroraboot/build.sh <IMAGE> <OUT_DIR> [ISO_NAME]
#
# e.g. auroraboot/build.sh sway-desktop:dev build/sway-desktop/iso
#
# Env:
#   ARCH                        target arch (default amd64)
#   AURORA_IMAGE                AuroraBoot image (default matches the Makefile pin)
#   HADRON_EXTRA_LIVE_CMDLINE   extra args appended to every live menuentry
#   STAGE_ONLY=1                stage the overlay dir and exit (used by tests)
#
# AuroraBoot reads the source image straight from the host Docker daemon via the
# mounted socket (`docker:` reference), so no intermediate registry is needed.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${1:?usage: build.sh <IMAGE> <OUT_DIR> [ISO_NAME]}"
OUT_DIR="${2:?usage: build.sh <IMAGE> <OUT_DIR> [ISO_NAME]}"
ISO_NAME="${3:-}"
ARCH="${ARCH:-amd64}"
AURORA_IMAGE="${AURORA_IMAGE:-quay.io/kairos/auroraboot:v0.21.0-alpha.4}"

THEME_SRC="rootfs/etc/kairos/branding/hadron-theme"
OVERLAY_DIR="$OUT_DIR/iso-overlay"

mkdir -p "$OUT_DIR"

# --- stage the --overlay-iso directory -------------------------------------
# AuroraBoot's build-iso only writes its default /boot/grub2/grub.cfg (the
# hardcoded "Kairos" menu) when that file does not already exist, so overlaying
# our own makes it keep ours. AuroraBoot substitutes {{NOMODESET}} and
# {{EXTEND_CMDLINE}} only on its own default and NOT on an overlaid file, so we
# reproduce that substitution here per-arch (see AuroraBoot pkg/ops/iso.go
# prepareBootArtifacts: amd64/x86 -> " nomodeset").
echo "==> staging live GRUB menu + theme (arch=$ARCH)"
case "$ARCH" in
  amd64|x86_64) NOMODESET=" nomodeset" ;;
  *)            NOMODESET="" ;;
esac
EXTEND_CMDLINE="${HADRON_EXTRA_LIVE_CMDLINE:+ ${HADRON_EXTRA_LIVE_CMDLINE}}"

rm -rf "$OVERLAY_DIR"
mkdir -p "$OVERLAY_DIR/boot/grub2/themes/hadron"
sed -e "s|{{NOMODESET}}|${NOMODESET}|g" \
    -e "s|{{EXTEND_CMDLINE}}|${EXTEND_CMDLINE}|g" \
    auroraboot/live-grub.cfg.tmpl > "$OVERLAY_DIR/boot/grub2/grub.cfg"

# Ship the Tokyo Night theme alongside the menu so the LIVE boot is themed too.
# Same three files the installed system gets from the 09_hadron_grub_theme oem
# stage — one source of truth for both. The font must travel with the theme:
# theme.txt references "Unifont Regular 16", which only exists once our vendored
# unicode.pf2 is loaded. The background is TGA because GRUB's png.mod rejects
# valid PNGs with "unsupported format".
cp "$THEME_SRC/theme.txt" "$THEME_SRC/unicode.pf2" "$THEME_SRC/background.tga" \
   "$OVERLAY_DIR/boot/grub2/themes/hadron/"

# theme.txt carries an @VARIANT_SUBTITLE@ placeholder that the Dockerfile
# substitutes inside the image. The copy we ship on the ISO comes from the repo
# working tree, so substitute it here too — an unrendered placeholder would be
# drawn literally on the boot menu. Default to the sway wording unless the image
# tag says otherwise.
case "$IMAGE" in
  i3-*|*/i3-*)   SUBTITLE="i3 · xlibre · kairos" ;;
  agent-*|*/agent-*) SUBTITLE="agent · xlibre · kairos" ;;
  *)             SUBTITLE="sway · wayland · kairos" ;;
esac
sed -i "s|@VARIANT_SUBTITLE@|${SUBTITLE}|" "$OVERLAY_DIR/boot/grub2/themes/hadron/theme.txt"

# --- GRUB modules the themed menu needs ------------------------------------
# The ISO's GRUB core image carries only its built-in modules, and gfxmenu --
# which `set theme` requires -- is NOT one of them. Without this the live menu
# prints
#     error: file `/boot/grub2/x86_64-efi/gfxmenu.mod' not found
#     error: module `gfxmenu' isn't loaded
# and falls back to the plain text menu. GRUB's prefix on the ISO is
# ($root)/boot/grub2 (see the ISO's /EFI/BOOT/grub.cfg, which sets prefix and
# then configfile's our grub.cfg), so dropping the matching module tree at
# /boot/grub2/<platform>/ is all it takes for insmod/gfxmenu to resolve.
#
# The modules come from the SAME image the ISO is built from, so their GRUB
# version can never drift from the core image that loads them. i386-pc is
# staged too so a legacy-BIOS boot of the same ISO gets the theme as well.
# Best-effort, like every other asset here: if the image ships no module tree
# we warn and carry on, and the `if [ -f theme.txt ]` guard in grub.cfg leaves
# GRUB on the plain text menu rather than blocking boot.
case "$ARCH" in
  amd64|x86_64)  GRUB_PLATFORMS="x86_64-efi i386-pc" ;;
  arm64|aarch64) GRUB_PLATFORMS="arm64-efi" ;;
  *)             GRUB_PLATFORMS="" ;;
esac
for platform in $GRUB_PLATFORMS; do
  mkdir -p "$OVERLAY_DIR/boot/grub2/$platform"
  # *.mod are the modules; *.lst (moddep.lst above all) is the dependency and
  # command index GRUB needs to resolve an insmod to a file.
  if docker run --rm --entrypoint sh "$IMAGE" -c \
       "cd /usr/lib/grub/$platform 2>/dev/null && tar cf - *.mod *.lst" \
       2>/dev/null | tar xf - -C "$OVERLAY_DIR/boot/grub2/$platform" 2>/dev/null; then
    echo "==> staged $(ls "$OVERLAY_DIR/boot/grub2/$platform"/*.mod | wc -l) $platform GRUB modules"
  else
    echo "!! no $platform GRUB modules in $IMAGE; live menu will fall back to text" >&2
    rmdir "$OVERLAY_DIR/boot/grub2/$platform" 2>/dev/null || true
  fi
done

if [ "${STAGE_ONLY:-0}" = "1" ]; then
  echo "==> staged $OVERLAY_DIR (STAGE_ONLY=1, not building an ISO)"
  exit 0
fi

# --- build --------------------------------------------------------------
rm -f "$OUT_DIR"/*.iso
echo "==> building ISO with AuroraBoot (source: docker:$IMAGE)"
docker run --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/$OUT_DIR:/output" \
  -v "$PWD/$OVERLAY_DIR:/overlay:ro" \
  "$AURORA_IMAGE" build-iso \
    --output /output/ \
    --overlay-iso /overlay \
    ${ISO_NAME:+--override-name "$ISO_NAME"} \
    "docker:$IMAGE"

ISO="$(ls -t "$OUT_DIR"/*.iso 2>/dev/null | head -1)"
[ -n "$ISO" ] || { echo "no ISO produced"; exit 1; }
echo "==> $ISO"
ls -lh "$ISO"
