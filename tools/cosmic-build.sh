#!/bin/sh
# Build one COSMIC component from its epoch tag and install it under $COSMIC_OUT.
#
#   cosmic-build.sh <repo> [just recipe]
#
# Used by the cosmic stages of the Dockerfile, one RUN per component, so a
# failure re-runs only that component. Every component's justfile takes
# `rootdir` and honours CARGO_TARGET_DIR, which the Dockerfile points at a
# BuildKit cache mount shared by all components: they pin the same libcosmic
# and iced revisions within an epoch, so most of the dependency graph compiles
# once. Nothing from the target dir reaches a layer.
#
# COSMIC_CARGO_ARGS adds arguments to the single `cargo build`. A component
# whose workspace members must not share one cargo invocation sets
# COSMIC_BUILD_RECIPE to a just recipe of its own justfile (with arguments),
# which then replaces the `cargo build`.
#
# Isotope's patches live in $COSMIC_PATCHES (default /usr/share/isotope-patches),
# see patches/cosmic/README.md: <repo>/*.patch is applied to the unpacked source,
# libcosmic/*.patch to every libcosmic git checkout the Cargo.lock names. When
# there are patches CARGO_TARGET_DIR gets a hash suffix, so a cached target dir
# cannot hold unpatched code.
#
# If <repo> is an existing directory it is copied instead of downloaded.
#
# COSMIC_INSTALL_CMD replaces the `just rootdir=... <recipe>` install (a shell
# command, evaluated in the source dir with the hashed CARGO_TARGET_DIR), for a
# component that installs with make.
#
# Every installed ELF is checked with ldd; a library that only the build stage
# had would otherwise surface as a desktop that fails to start.
set -eu

repo=$1
recipe=${2:-install}
: "${COSMIC_VERSION:?}" "${COSMIC_OUT:?}"

patches=${COSMIC_PATCHES:-/usr/share/isotope-patches}
build=${COSMIC_BUILD_DIR:-/build}

src=
if [ -d "$repo" ]; then
    src=$(cd "$repo" && pwd)
    repo=$(basename "$src")
fi

if [ -n "$src" ] && [ "$src" = "$(cd "$build" && pwd)/$repo" ]; then
    echo "cosmic-build: $src is the build directory of $repo; refusing to delete it" >&2
    exit 1
fi

cd "$build"
rm -rf "$repo"
mkdir "$repo"
if [ -n "$src" ]; then
    cp -a "$src"/. "$repo"/
else
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors \
        "https://github.com/pop-os/${repo}/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz" |
        tar -xz --strip-components=1 -C "$repo"
fi
cd "$repo"

# Patches, in name order (a glob sorts).
repo_patches=
for p in "$patches/$repo"/*.patch; do
    [ -f "$p" ] && repo_patches="$repo_patches $p"
done
lib_patches=
for p in "$patches"/libcosmic/*.patch; do
    [ -f "$p" ] && lib_patches="$lib_patches $p"
done
# shellcheck disable=SC2086
if [ -n "$repo_patches$lib_patches" ]; then
    all_hash=$(for p in "$patches"/*/*.patch; do [ -f "$p" ] && echo "${p#"$patches"/}" && cat "$p"; done | sha256sum | cut -c1-12)
    if [ -n "${CARGO_TARGET_DIR:-}" ]; then
        export CARGO_TARGET_DIR="$CARGO_TARGET_DIR/$all_hash"
    fi
fi
for p in $repo_patches; do
    echo "cosmic-build: patching $repo with ${p##*/}" >&2
    patch -p1 -i "$p"
done

# libcosmic is a git dependency: patch every checkout this Cargo.lock names.
# A lock that names none (cosmic-session, cosmic-randr, ...) has nothing to patch.
if [ -n "$lib_patches" ]; then
    # cargo names the checkout dir after the first 7 hex of the rev.
    revs=$(sed -n 's|^source = "git+https://github.com/pop-os/libcosmic[^#]*#\([0-9a-f]\{7\}\).*|\1|p' Cargo.lock | sort -u)
    if [ -n "$revs" ]; then
        cargo fetch --locked
        # shellcheck disable=SC2086
        lib_hash=$(cat $lib_patches | sha256sum | cut -d' ' -f1)
        checkouts="${CARGO_HOME:-$HOME/.cargo}/git/checkouts"
        for rev in $revs; do
            n=0
            for dir in "$checkouts"/libcosmic-*/"$rev"*; do
                [ -d "$dir" ] || continue
                n=$((n + 1))
                [ -e "$dir/.isotope-patched-$lib_hash" ] && continue
                for p in $lib_patches; do
                    echo "cosmic-build: patching libcosmic $rev with ${p##*/}" >&2
                    (cd "$dir" && patch -p1 -i "$p")
                done
                : > "$dir/.isotope-patched-$lib_hash"
            done
            if [ "$n" -eq 0 ]; then
                echo "cosmic-build: Cargo.lock names libcosmic $rev but no checkout of it was found" >&2
                exit 1
            fi
        done
    fi
fi

# vergen asks git for these; there is no .git in a tarball.
export VERGEN_GIT_COMMIT_DATE="1970-01-01" VERGEN_GIT_SHA="epoch-${COSMIC_VERSION}"

if [ -n "${COSMIC_BUILD_RECIPE:-}" ]; then
    just ${COSMIC_BUILD_RECIPE}
else
    cargo build --release --locked ${COSMIC_CARGO_ARGS:-}
fi
mkdir -p "$COSMIC_OUT"
if [ -n "${COSMIC_INSTALL_CMD:-}" ]; then
    eval "$COSMIC_INSTALL_CMD"
else
    just rootdir="$COSMIC_OUT" "$recipe"
fi

find "$COSMIC_OUT" -type f -perm -u+x | while read -r f; do
    if file "$f" 2>/dev/null | grep -q ELF || head -c4 "$f" | grep -q ELF; then
        if ldd "$f" 2>&1 | grep -q 'not found'; then
            echo "missing libraries for $f:" >&2
            ldd "$f" | grep 'not found' >&2
            exit 1
        fi
    fi
done

cd "$build"
rm -rf "$repo"
