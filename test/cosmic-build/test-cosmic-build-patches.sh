#!/bin/sh
# Checks the patch support of tools/cosmic-build.sh with stubbed curl, cargo and
# just, no network and no docker:
#
# - patches/<repo>/*.patch is applied to the unpacked source;
# - libcosmic/*.patch is applied once to the libcosmic checkout the Cargo.lock
#   names, and a second build does not apply it again;
# - a patch that does not apply fails the build;
# - CARGO_TARGET_DIR, as cargo sees it, ends in 12 hex when there are patches
#   and is untouched when there are none;
# - a directory argument is copied instead of downloaded;
# - applets/Cargo.lock (Isotope's own applets) names only libcosmic revs that
#   patches/cosmic/README.md lists, the revs the libcosmic patches are kept for.
#
#   test/cosmic-build/test-cosmic-build-patches.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=$(cd "$here/../.." && pwd)/tools/cosmic-build.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { echo "ok: $*"; }
bad() { echo "FAIL: $*" >&2; fail=1; }

mkdir -p "$work/bin" "$work/build" "$work/out" "$work/cargo/git/checkouts/libcosmic-x/abc1234" "$work/fixture/fake-repo"
cd "$work/fixture/fake-repo"
printf 'hello\n' > hello.txt
cat > Cargo.lock <<'LOCK'
[[package]]
name = "libcosmic"
source = "git+https://github.com/pop-os/libcosmic?rev=abc1234#abc1234def5678abc1234def5678abc1234def56"
[[package]]
name = "cosmic-config"
source = "git+https://github.com/pop-os/libcosmic?rev=abc1234#abc1234def5678abc1234def5678abc1234def56"
LOCK
(cd "$work/fixture" && tar -czf "$work/fake-repo.tar.gz" fake-repo)
printf 'lib\n' > "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt"

cat > "$work/bin/curl" <<STUB
#!/bin/sh
cat "$work/fake-repo.tar.gz"
STUB
cat > "$work/bin/cargo" <<STUB
#!/bin/sh
echo "\$1 \$CARGO_TARGET_DIR" >> "$work/cargo.log"
STUB
printf '#!/bin/sh\nexit 0\n' > "$work/bin/just"
chmod +x "$work/bin/"*

mkdir -p "$work/patches/fake-repo" "$work/patches/libcosmic"
cat > "$work/patches/fake-repo/0001-hello.patch" <<'P'
--- a/hello.txt
+++ b/hello.txt
@@ -1 +1 @@
-hello
+hello isotope
P
cat > "$work/patches/libcosmic/0001-lib.patch" <<'P'
--- a/lib.txt
+++ b/lib.txt
@@ -1 +1 @@
-lib
+lib isotope
P

run() {
    PATH="$work/bin:$PATH" COSMIC_VERSION=1 COSMIC_OUT="$work/out" COSMIC_BUILD_DIR="$work/build" \
        CARGO_HOME="$work/cargo" COSMIC_PATCHES="${PATCHES:-$work/patches}" CARGO_TARGET_DIR="$work/target" \
        sh "$script" "$@"
}
# The build removes the unpacked source, so keep a copy of what the repo
# patch produced through a just stub that records it.
cat > "$work/bin/just" <<STUB
#!/bin/sh
cp hello.txt "$work/hello.seen"
STUB

run fake-repo >"$work/log1" 2>&1 || { cat "$work/log1"; bad "build with patches failed"; }
[ "$(cat "$work/hello.seen" 2>/dev/null)" = "hello isotope" ] && ok "repo patch applied" || bad "repo patch not applied"
[ "$(cat "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt")" = "lib isotope" ] && ok "libcosmic patch applied" || bad "libcosmic patch not applied"
ls "$work/cargo/git/checkouts/libcosmic-x/abc1234"/.isotope-patched-* >/dev/null 2>&1 && ok "stamp written" || bad "no stamp"

run fake-repo >"$work/log2" 2>&1 || { cat "$work/log2"; bad "second build failed"; }
[ "$(grep -c 'patching libcosmic' "$work/log1" "$work/log2" | tr '\n' ' ')" = "$work/log1:1 $work/log2:0 " ] \
    && ok "libcosmic patch applied once across two builds" || bad "libcosmic patch applied again or never"

grep -Eq "^build $work/target/[0-9a-f]{12}\$" "$work/cargo.log" && ok "CARGO_TARGET_DIR ends in 12 hex" || bad "no hashed CARGO_TARGET_DIR: $(cat "$work/cargo.log")"

# A broken patch fails the build.
printf 'lib\n' > "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt"
rm -f "$work/cargo/git/checkouts/libcosmic-x/abc1234"/.isotope-patched-*
cat > "$work/patches/libcosmic/0002-broken.patch" <<'P'
--- a/nothere.txt
+++ b/nothere.txt
@@ -1 +1 @@
-x
+y
P
if run fake-repo >"$work/log3" 2>&1; then bad "broken patch did not fail the build"; else ok "broken patch fails the build"; fi
rm "$work/patches/libcosmic/0002-broken.patch"

# No patches: a missing dir and an empty dir leave CARGO_TARGET_DIR alone.
: > "$work/cargo.log"
mkdir "$work/empty"
for d in "$work/missing" "$work/empty"; do
    PATCHES=$d run fake-repo >"$work/log4" 2>&1 || { cat "$work/log4"; bad "build with patches dir $d failed"; }
done
[ "$(sort -u "$work/cargo.log")" = "build $work/target" ] && ok "no patches: CARGO_TARGET_DIR untouched" || bad "target dir changed without patches: $(cat "$work/cargo.log")"

# Directory mode (the checkout the failed build half-patched is reset first).
printf 'lib\n' > "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt"
rm -f "$work/cargo/git/checkouts/libcosmic-x/abc1234"/.isotope-patched-*
mkdir -p "$work/applets" "$work/patches/applets"
printf 'hello\n' > "$work/applets/hello.txt"
cp "$work/fixture/fake-repo/Cargo.lock" "$work/applets/"
cp "$work/patches/fake-repo/0001-hello.patch" "$work/patches/applets/"
run "$work/applets" >"$work/log5" 2>&1 || { cat "$work/log5"; bad "directory mode failed"; }
[ "$(cat "$work/hello.seen")" = "hello isotope" ] && ok "directory copied and patched" || bad "directory mode did not patch"

# A lock that names no libcosmic: passes, nothing patched.
mkdir -p "$work/nolib"
printf 'hello\n' > "$work/nolib/hello.txt"
printf '[[package]]\nname = "x"\n' > "$work/nolib/Cargo.lock"
printf 'lib\n' > "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt"
rm -f "$work/cargo/git/checkouts/libcosmic-x/abc1234"/.isotope-patched-*
run "$work/nolib" >"$work/log6" 2>&1 || { cat "$work/log6"; bad "lock without libcosmic failed"; }
[ "$(cat "$work/cargo/git/checkouts/libcosmic-x/abc1234/lib.txt")" = "lib" ] && ok "lock without libcosmic: nothing patched" || bad "libcosmic patched without being named"

# Two revs named, one checkout missing: fails.
mkdir -p "$work/tworev"
printf 'hello\n' > "$work/tworev/hello.txt"
cat > "$work/tworev/Cargo.lock" <<'LOCK'
[[package]]
name = "libcosmic"
source = "git+https://github.com/pop-os/libcosmic?rev=abc1234#abc1234def5678abc1234def5678abc1234def56"
[[package]]
name = "iced"
source = "git+https://github.com/pop-os/libcosmic?rev=fff9999#fff9999def5678abc1234def5678abc1234def56"
LOCK
if run "$work/tworev" >"$work/log7" 2>&1; then bad "missing second checkout did not fail"; else ok "a named rev without a checkout fails"; fi

# The source dir may not be the build dir's own copy.
mkdir -p "$work/build/self"
if run "$work/build/self" >"$work/log8" 2>&1; then bad "self-copy not refused"; else ok "source equal to build dir refused"; fi
[ -d "$work/build/self" ] && ok "source dir survived" || bad "source dir deleted"

# COSMIC_INSTALL_CMD replaces the just install, with the hashed target dir.
COSMIC_INSTALL_CMD='echo "$CARGO_TARGET_DIR" > "$COSMIC_OUT/inst"' run fake-repo >"$work/log9" 2>&1 || { cat "$work/log9"; bad "install cmd build failed"; }
grep -Eq "^$work/target/[0-9a-f]{12}\$" "$work/out/inst" && ok "install command ran with hashed target dir" || bad "install command not run"

# Isotope's applets must stay on a libcosmic rev the patches are kept for:
# patches/cosmic/README.md's table lists them (12 hex, in backticks). The revs
# are read with cosmic-build.sh's own sed, so this sees what the build patches.
repo=$(cd "$here/../.." && pwd)
lock_revs() {
    sed -n 's|^source = "git+https://github.com/pop-os/libcosmic[^#]*#\([0-9a-f]\{7\}\).*|\1|p' "$1" | sort -u
}
listed_revs() {
    # shellcheck disable=SC2016 # the backticks of the Markdown table, not a command
    sed -n 's/^| `\([0-9a-f]\{12\}\)` |.*/\1/p' "$repo/patches/cosmic/README.md"
}
# Prints each rev of lock $1 that the README does not list; fails if there is
# one, or if the lock names no libcosmic at all.
unlisted_revs() {
    revs=$(lock_revs "$1")
    [ -n "$revs" ] || { echo "(no libcosmic in $1)"; return 1; }
    rc=0
    for r in $revs; do
        listed_revs | grep -q "^$r" || { echo "$r"; rc=1; }
    done
    return $rc
}
[ -n "$(listed_revs)" ] && ok "patches/cosmic/README.md lists libcosmic revs" || bad "no libcosmic revs found in patches/cosmic/README.md"
if out=$(unlisted_revs "$repo/applets/Cargo.lock"); then
    ok "applets/Cargo.lock names only listed libcosmic revs ($(lock_revs "$repo/applets/Cargo.lock" | tr '\n' ' '))"
else
    bad "applets/Cargo.lock names libcosmic revs patches/cosmic/README.md does not list: $out"
fi
# The check itself: a lock on an unlisted rev fails.
printf '[[package]]\nname = "libcosmic"\nsource = "git+https://github.com/pop-os/libcosmic#0000000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"\n' > "$work/unlisted.lock"
if unlisted_revs "$work/unlisted.lock" >/dev/null; then bad "a lock on an unlisted libcosmic rev passed"; else ok "a lock on an unlisted libcosmic rev fails"; fi

exit $fail
