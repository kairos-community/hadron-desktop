#!/usr/bin/env bash
# Build the UI E2E fixtures for the agent appliance's `ui` gate.
#
# The GTK fixture is compiled AGAINST THE AGENT RUNTIME, not against the host or
# a stock toolchain image: the gate asserts what the appliance's own GTK and
# AT-SPI stack reports, so a binary linked against a different GTK would be
# testing something the appliance never runs.
#
# The mechanism is the one Phase 1 already established in
# test/agent/Dockerfile.compat: build in ghcr.io/kairos-io/hadron-toolchain,
# with the runtime image's headers, libraries and pkg-config data copied in. The
# agent image itself carries no compiler -- it is a runtime, and should stay one.
#
# Output is a deterministic tar plus its SHA-256 under the ignored runtime
# directory, so the `ui` gate uploads a byte-identical, verifiable payload
# instead of rebuilding inside the guest.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
IMAGE="${AGENT_IMAGE:-agent-desktop:dev}"
TOOLCHAIN="${FIXTURE_TOOLCHAIN:-ghcr.io/kairos-io/hadron-toolchain:main}"
OUT_DIR="${FIXTURE_OUT:-$SCRIPT_DIR/runtime/fixtures}"
SRC="$SCRIPT_DIR/fixtures/gtk3/main.c"
HTML="$SCRIPT_DIR/fixtures/chromium/index.html"
COMMIT="$SCRIPT_DIR/fixtures/chromium.commit"

info() { echo -e "\033[1;34m[fixtures]\033[0m $*" >&2; }
die()  { echo -e "\033[1;31m[fixtures] $*\033[0m" >&2; exit 1; }

for f in "$SRC" "$HTML" "$COMMIT"; do
  [ -f "$f" ] || die "missing fixture input: $f"
done
command -v docker >/dev/null || die "docker is required to build against the agent runtime"
docker image inspect "$IMAGE" >/dev/null 2>&1 \
  || die "agent image $IMAGE not found; run 'make agent-image' first"

mkdir -p "$OUT_DIR"; chmod 700 "$OUT_DIR" 2>/dev/null || true
stage="$(mktemp -d)"; trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/bin" "$stage/web"

info "Compiling the GTK fixture against $IMAGE"
# The build stage mirrors Dockerfile.compat's fixture-build: the runtime's
# /usr/include, /usr/lib and pkgconfig are copied over the toolchain so the
# fixture links against exactly the GTK the appliance ships. rmdir of the
# firmware directory matches the production build, where the runtime carries a
# symlink the toolchain has as an empty directory.
if ! docker build --iidfile "$stage/iid" -f - "$REPO_ROOT" >"$stage/build.log" 2>&1 <<DOCKERFILE
ARG BASE_IMAGE=$IMAGE
FROM \${BASE_IMAGE} AS fixture-runtime

FROM $TOOLCHAIN AS fixture-build
RUN rmdir /usr/lib/firmware 2>/dev/null || true
COPY --from=fixture-runtime /usr/include/ /usr/include/
COPY --from=fixture-runtime /usr/lib/ /usr/lib/
COPY --from=fixture-runtime /usr/share/pkgconfig/ /usr/share/pkgconfig/
ENV PKG_CONFIG_PATH=/usr/lib/pkgconfig:/usr/share/pkgconfig
COPY test/agent/fixtures/gtk3/main.c /src/main.c
RUN mkdir -p /out && cc -O2 -Wall -Wextra \$(pkg-config --cflags gtk+-3.0) \\
      /src/main.c -o /out/hadron-cua-gtk \$(pkg-config --libs gtk+-3.0)

FROM scratch
COPY --from=fixture-build /out/hadron-cua-gtk /hadron-cua-gtk
DOCKERFILE
then
  cat "$stage/build.log" >&2
  die "GTK fixture build failed"
fi

iid="$(cat "$stage/iid")"
cid="$(docker create "$iid" /nonexistent)"
docker cp "$cid:/hadron-cua-gtk" "$stage/bin/hadron-cua-gtk" >/dev/null
docker rm -f "$cid" >/dev/null 2>&1 || true
docker rmi -f "$iid" >/dev/null 2>&1 || true

[ -s "$stage/bin/hadron-cua-gtk" ] || die "the fixture build produced an empty binary"
chmod 755 "$stage/bin/hadron-cua-gtk"
cp "$HTML" "$stage/web/index.html"
cp "$COMMIT" "$stage/web/chromium.commit"

tar="$OUT_DIR/ui-fixtures.tar"
# Deterministic tar (fixed owner, sorted names, fixed mtime) so an unchanged
# fixture yields an unchanged checksum: CI caches on it, and a checksum that
# drifted for no reason would silently defeat that.
tar --sort=name --owner=0 --group=0 --numeric-owner \
    --mtime='UTC 2020-01-01' -C "$stage" -cf "$tar" bin web
sha256sum "$tar" | awk '{print $1}' > "$tar.sha256"

info "Wrote $tar"
info "  sha256: $(cat "$tar.sha256")"
