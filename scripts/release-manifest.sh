#!/usr/bin/env bash
# Generate a deterministic build manifest for one release image.
#
# Usage: release-manifest.sh <flavor> <release-tag> <image> <iso-asset-path> <out.json>
#
#   flavor          sway | i3 | agent
#   release-tag     the release tag being published, e.g. v0.4.0
#   image           the final OCI image, e.g. i3-desktop:dev
#   iso-asset-path  path to the ISO; its sibling .sha256 must already exist
#   out.json        where to write the manifest
#
# Every field is read back from the thing that produced it -- the Makefile, the
# Dockerfiles, the built image -- rather than hardcoded here, so a manifest
# cannot quietly drift from what was actually built. Any field that cannot be
# resolved is a hard failure, EXCEPT the ones that legitimately do not apply to
# a flavor: those are emitted as JSON null rather than as an empty string, so a
# consumer can tell "not applicable" from "we failed to look".
#
# Sway is Wayland-only and links no X server, so it carries no XLibre version.
# Only the agent image carries Cua and AT-SPI.
set -euo pipefail

die() { echo "release-manifest: $*" >&2; exit 1; }

[ "$#" -eq 5 ] || die "usage: $0 <flavor> <release-tag> <image> <iso-asset-path> <out.json>"
flavor="$1" release_tag="$2" image="$3" iso_path="$4" out="$5"

case "$flavor" in
  sway|i3|agent) ;;
  *) die "unknown flavor: $flavor (want sway, i3, or agent)" ;;
esac
[ -f "$iso_path" ]           || die "no ISO at $iso_path"
[ -f "$iso_path.sha256" ]    || die "no checksum at $iso_path.sha256"
command -v jq >/dev/null     || die "jq is required"
command -v docker >/dev/null || die "docker is required"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Makefile: `BASE_IMAGE ?= ghcr.io/kairos-io/hadron:main`.
base_ref="$(grep -oP '^BASE_IMAGE\s*\?=\s*\K\S+' "$repo_root/Makefile" || true)"
[ -n "$base_ref" ] || die "could not read BASE_IMAGE from the Makefile"

# The base was pulled by `docker build`, so it carries the digest it resolved to.
base_digest="$(
  docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$base_ref" 2>/dev/null |
    head -n1 | cut -d@ -f2
)"
[[ "$base_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "could not resolve a registry digest for $base_ref"

image_id="$(docker image inspect --format '{{.Id}}' "$image")" \
  || die "could not inspect the built image $image"
iso_sha256="$(cut -d' ' -f1 < "$iso_path.sha256")"
[ -n "$iso_sha256" ] || die "empty checksum in $iso_path.sha256"

git_commit="$(cd "$repo_root" && git rev-parse HEAD)"

# Dockerfile: `ARG XLIBRE_VERSION=...`. Absent from the Sway variant by design.
xlibre_version=null
if [ "$flavor" = "i3" ] || [ "$flavor" = "agent" ]; then
  raw="$(grep -oP '^ARG XLIBRE_VERSION=\K\S+' "$repo_root/Dockerfile" | head -n1 || true)"
  [ -n "$raw" ] || die "could not read ARG XLIBRE_VERSION from the Dockerfile"
  xlibre_version="$(jq -n --arg v "$raw" '$v')"
fi

# Dockerfile.agent: `ARG CUA_COMMIT=...` appears in more than one stage. They
# must agree, or the manifest would name a revision only half the image was
# built from.
cua_revision=null
atspi_version=null
if [ "$flavor" = "agent" ]; then
  mapfile -t cua_all < <(grep -oP '^ARG CUA_COMMIT=\K\S+' "$repo_root/Dockerfile.agent" | sort -u)
  [ "${#cua_all[@]}" -eq 1 ] \
    || die "Dockerfile.agent declares ${#cua_all[@]} distinct CUA_COMMIT values; expected exactly one"
  cua_revision="$(jq -n --arg v "${cua_all[0]}" '$v')"

  raw="$(grep -oP '^ARG ATSPI_VERSION=\K\S+' "$repo_root/Dockerfile.agent" | head -n1 || true)"
  [ -n "$raw" ] || die "could not read ARG ATSPI_VERSION from Dockerfile.agent"
  atspi_version="$(jq -n --arg v "$raw" '$v')"
fi

jq -n \
  --arg release_tag "$release_tag" \
  --arg git_commit "$git_commit" \
  --arg hadron_base_reference "$base_ref" \
  --arg hadron_base_digest "$base_digest" \
  --arg image_id "$image_id" \
  --arg iso_sha256 "$iso_sha256" \
  --arg desktop_flavor "$flavor" \
  --argjson xlibre_version "$xlibre_version" \
  --argjson cua_revision "$cua_revision" \
  --argjson atspi_version "$atspi_version" \
  '{
     release_tag: $release_tag,
     git_commit: $git_commit,
     hadron_base_reference: $hadron_base_reference,
     hadron_base_digest: $hadron_base_digest,
     image_id: $image_id,
     iso_sha256: $iso_sha256,
     desktop_flavor: $desktop_flavor,
     xlibre_version: $xlibre_version,
     cua_revision: $cua_revision,
     atspi_version: $atspi_version
   }' > "$out"

echo "release-manifest: wrote $out" >&2
