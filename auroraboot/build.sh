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
# drawn literally on the boot menu.
#
# Ask the IMAGE, don't guess from its tag. /etc/hadron-desktop/session is the
# same source of truth the Dockerfile uses for the installed system's theme, so
# reading it here keeps the live menu and the installed menu in agreement. A
# tag-shaped heuristic cannot: the agent variant is tagged `agent-*` but runs i3,
# so guessing gave it "agent · xlibre · kairos" live and "i3 · xlibre · kairos"
# once installed. Same lowercasing as the Dockerfile.
#
# The `|| true` matters: under `set -e -o pipefail` a failing docker run (no
# session file, image won't start) makes the assignment itself nonzero and the
# script dies right here with no diagnostic. Swallowing it lets the explicit
# check below report WHY the build stopped.
SUBTITLE="$(docker run --rm --entrypoint sh "$IMAGE" -c \
  '. /etc/hadron-desktop/session && printf "%s · %s · kairos" "$DESKTOP_NAME" "$DISPLAY_NAME"' \
  2>/dev/null | tr '[:upper:]' '[:lower:]' || true)"
# Hard failure, unlike the best-effort GRUB assets below: a theme with an empty
# or literal-placeholder subtitle draws that on the boot menu of every install.
# The pattern also rejects "$SUBTITLE" with an empty DESKTOP_NAME/DISPLAY_NAME,
# which a bare -z test would let through as " ·  · kairos".
if [[ ! "$SUBTITLE" =~ ^[^[:space:]]+\ ·\ [^[:space:]]+\ ·\ kairos$ ]]; then
  echo "!! could not read a subtitle from /etc/hadron-desktop/session in $IMAGE" \
       "(got: '$SUBTITLE')" >&2
  exit 1
fi
echo "==> live menu subtitle: $SUBTITLE"
sed -i "s|@VARIANT_SUBTITLE@|${SUBTITLE}|" "$OVERLAY_DIR/boot/grub2/themes/hadron/theme.txt"
if grep -q '@VARIANT_SUBTITLE@' "$OVERLAY_DIR/boot/grub2/themes/hadron/theme.txt"; then
  echo "!! @VARIANT_SUBTITLE@ survived substitution" >&2
  exit 1
fi

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
# GRUB refuses to load a module built by a different GRUB version than the core
# image loading it, so each platform's modules must come from whichever image
# actually produced that platform's core:
#
#   *-efi   -> $IMAGE. AuroraBoot copies the build image's own
#              /usr/lib/grub/x86_64-efi/grubx64.efi onto the ISO, so the EFI core
#              and these modules are the same GRUB build by construction.
#   i386-pc -> $AURORA_IMAGE. AuroraBoot builds the eltorito BIOS core itself,
#              inside its own container, with its own grub2-mkimage. That is a
#              DIFFERENT GRUB than the build image's: today AuroraBoot
#              v0.21.0-alpha.4 is GRUB 2.12 while the desktop image is 2.14, and
#              gfxmenu.mod differs between them. Staging $IMAGE's i386-pc tree
#              here would hand a 2.12 core a set of 2.14 modules.
#
# Best-effort, like every other asset here: if a source tree is absent we warn
# and carry on, and the `if [ -f theme.txt ]` guard in grub.cfg leaves GRUB on
# the plain text menu rather than blocking boot.
case "$ARCH" in
  amd64|x86_64)  GRUB_PLATFORMS="x86_64-efi:$IMAGE i386-pc:$AURORA_IMAGE" ;;
  arm64|aarch64) GRUB_PLATFORMS="arm64-efi:$IMAGE" ;;
  *)             GRUB_PLATFORMS="" ;;
esac
for spec in $GRUB_PLATFORMS; do
  platform="${spec%%:*}"
  src="${spec#*:}"
  mkdir -p "$OVERLAY_DIR/boot/grub2/$platform"
  # *.mod are the modules; *.lst (moddep.lst above all) is the dependency and
  # command index GRUB needs to resolve an insmod to a file.
  if docker run --rm --entrypoint sh "$src" -c \
       "cd /usr/lib/grub/$platform 2>/dev/null && tar cf - *.mod *.lst" \
       2>/dev/null | tar xf - -C "$OVERLAY_DIR/boot/grub2/$platform" 2>/dev/null; then
    echo "==> staged $(ls "$OVERLAY_DIR/boot/grub2/$platform"/*.mod | wc -l) $platform GRUB modules from $src"
  else
    echo "!! no $platform GRUB modules in $src; live menu will fall back to text" >&2
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
