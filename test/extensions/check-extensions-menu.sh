#!/bin/sh
# The fakes are scripts written out in single quotes, on purpose.
# shellcheck disable=SC2016
# Tests for the Extensions entry of hadron-menu and the hadron-ext rows of
# hadron-help.
#
#   test/extensions/check-extensions-menu.sh
#
# Runs itself in an alpine container (it writes /etc/hadron-desktop/session),
# with fakes for hadron-ext, hadron-pick and the terminal (foot). It checks:
#   1. a catalog entry without ✓ runs `hadron-ext install NAME` in a terminal,
#      one with ✓ runs `hadron-ext remove NAME`, and "Install from a URL…"
#      runs `hadron-ext install -`;
#   2. a cancelled pick runs nothing, and a picked line whose name is not a
#      plain extension name never reaches the shell;
#   3. when hadron-ext cannot list the catalogs, a terminal shows why;
#   4. hadron-help lists hadron-ext only when it is installed;
#   5. the wiring: the hadron-ext link and the signature config in the
#      Dockerfile, and their removal in Dockerfile.agent.
set -u

repo="$(cd "$(dirname "$0")/../.." && pwd)"

if [ "${1:-}" != --inside ]; then
    exec docker run --rm -v "$repo:/src:ro" alpine:3.22 sh /src/test/extensions/check-extensions-menu.sh --inside
fi

fail=0
ok()  { echo "ok: $*"; }
err() { echo "FAIL: $*"; fail=1; }
expect() {
    if [ "$2" = "$3" ]; then ok "$1"; else err "$1: got [$2], want [$3]"; fi
}

T=$(mktemp -d)
B="$T/bin"
mkdir -p "$B" /etc/hadron-desktop
export T
PATH="$B:$PATH"
printf 'DESKTOP_NAME=Sway\n' > /etc/hadron-desktop/session

fake() { printf '#!/bin/sh\n%s\n' "$2" > "$B/$1"; chmod +x "$B/$1"; }
# hadron-ext browse --menu prints $T/list, or fails without it.
fake hadron-ext 'echo "hadron-ext $*" >> "$T/ext.log"
[ "$1 $2" = "browse --menu" ] || exit 0
[ -f "$T/list" ] || exit 1
cat "$T/list"'
# hadron-pick: prints line $T/choice of its input, or cancels.
fake hadron-pick 'cat > "$T/pick.in"
[ -f "$T/choice" ] || exit 1
sed -n "$(cat "$T/choice")p" "$T/pick.in"'
# foot --app-id=floatterm sh -c CMD: records CMD.
fake foot 'shift 3; printf "%s\n" "$1" > "$T/term"'

menu() {
    rm -f "$T/term" "$T/pick.in"
    "$repo/rootfs/usr/bin/hadron-menu" Extensions
    # term runs in the background.
    i=0; while [ ! -s "$T/term" ] && [ $i -lt 20 ]; do sleep 0.05; i=$((i + 1)); done
    sed 's/; printf.*//' "$T/term" 2>/dev/null
}

printf '  tailscale — VPN\n✓ gpg — GnuPG\n  bad;rm — x\nInstall from a URL…\n' > "$T/list"
echo 1 > "$T/choice"
expect "a new extension is installed in a terminal" "$(menu)" "hadron-ext install tailscale"
expect "the picker gets the catalog lines" "$(head -n 2 "$T/pick.in")" "  tailscale — VPN
✓ gpg — GnuPG"
echo 2 > "$T/choice"
expect "an installed extension is removed in a terminal" "$(menu)" "hadron-ext remove gpg"
echo 4 > "$T/choice"
expect "Install from a URL asks in a terminal" "$(menu)" "hadron-ext install -"
echo 3 > "$T/choice"
expect "a name with shell characters runs nothing" "$(menu)" ""
rm "$T/choice"
expect "a cancelled pick runs nothing" "$(menu)" ""
rm "$T/list"
expect "no catalog: a terminal shows why" "$(menu)" "hadron-ext browse"

# --- hadron-help ------------------------------------------------------------
printf '# x\nBasics\tSuper + F1\tThis help\n' > "$T/sheet"
expect "hadron-help lists hadron-ext" "$(HADRON_SHORTCUTS="$T/sheet" "$repo/rootfs/usr/bin/hadron-help" --text | grep -c 'hadron-ext')" 2
rm "$B/hadron-ext"
expect "hadron-help hides hadron-ext without it (agent)" "$(HADRON_SHORTCUTS="$T/sheet" "$repo/rootfs/usr/bin/hadron-help" --text | grep -c 'hadron-ext')" 0
expect "hadron-help still ends with the update rows" "$(HADRON_SHORTCUTS="$T/sheet" "$repo/rootfs/usr/bin/hadron-help" --text | tail -n 1 | grep -c 'hadron-update upgrade')" 1

# --- wiring -----------------------------------------------------------------
expect "the image links hadron-ext to hadron-update" "$(grep -c '^RUN ln -sf hadron-update /usr/bin/hadron-ext$' /src/Dockerfile)" 1
expect "the polkit file declares the extensions action" "$(grep -c '<action id="io.kairos.hadron-update.extensions">' /src/rootfs/usr/share/polkit-1/actions/io.kairos.hadron-update.policy)" 1
expect "the Kairos config ignores extension signatures" "$(grep -c '^  ignore_signatures: true$' /src/rootfs/etc/kairos/90_hadron_extensions.yaml)" 1
expect "the default catalog list names hadron-layers" "$(grep -c '^catalog=https://kairos-io.github.io/hadron-layers/releases.json$' /src/rootfs/etc/hadron-desktop/extensions.conf)" 1
expect "the agent image removes hadron-ext and the extension config" \
    "$(grep -c 'rm -f /usr/bin/hadron-ext /etc/hadron-desktop/extensions.conf /etc/kairos/90_hadron_extensions.yaml /system/oem/93_hadron_extensions.yaml' /src/Dockerfile.agent)" 1

rm -rf "$T"
[ "$fail" -eq 0 ] && echo "all extensions menu tests passed"
exit "$fail"
