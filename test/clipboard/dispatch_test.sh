#!/bin/sh
# The fakes are scripts written out in single quotes, on purpose.
# shellcheck disable=SC2016
# Tests for hadron-clipboard (the clipboard history) and its wiring.
#
#   test/clipboard/dispatch_test.sh
#
# Runs itself in an alpine container (it writes /etc/hadron-desktop/session),
# with fakes on PATH for every tool the script calls: noctalia, cliphist,
# wl-paste, wl-copy, hadron-pick, xclip, xsel and clipnotify. It checks:
#   1. the dispatch per DESKTOP_NAME (niri, Sway, COSMIC, i3) for show, daemon
#      and clear;
#   2. the i3 recorder: text only, no x-kde-passwordManagerHint copies, no
#      empty copies, a repeated copy moves to the top, the history is cut to
#      HADRON_CLIPBOARD_MAX, and the picker puts the exact text back;
#   3. the Clipboard entry of hadron-menu, and its absence without the script
#      (the agent image);
#   4. the session wiring: the recorder starts and Super+Ctrl+V is bound once
#      on every variant, and the agent image removes both.
set -u

repo="$(cd "$(dirname "$0")/../.." && pwd)"

if [ "${1:-}" != --inside ]; then
    exec docker run --rm -v "$repo:/src:ro" alpine:3.22 sh /src/test/clipboard/dispatch_test.sh --inside
fi

fail=0
ok()  { echo "ok: $*"; }
err() { echo "FAIL: $*"; fail=1; }
# expect DESC ACTUAL WANTED
expect() {
    if [ "$2" = "$3" ]; then ok "$1"; else err "$1: got [$2], want [$3]"; fi
}

T=$(mktemp -d)
B="$T/bin"
mkdir -p "$B" "$T/run"
export XDG_RUNTIME_DIR="$T/run" T
PATH="$B:$PATH"
ln -s "$repo/rootfs/usr/bin/hadron-clipboard" "$B/hadron-clipboard"
mkdir -p /etc/hadron-desktop

fake() { printf '#!/bin/sh\n%s\n' "$2" > "$B/$1"; chmod +x "$B/$1"; }
fake noctalia 'echo "noctalia $*" >> "$T/log"'
fake wl-paste 'echo "wl-paste $*" >> "$T/log"'
fake wl-copy  'cat > "$T/wl-copy.out"'
fake cliphist 'echo "cliphist $*" >> "$T/log"
for a; do last=$a; done
case "$last" in
    list)   printf "2\tsecond\n1\tfirst\n" ;;
    decode) printf "decoded:"; cat ;;
esac'
# hadron-pick: keeps its input, then prints line $T/choice, or cancels.
fake hadron-pick 'cat > "$T/pick.in"
[ -f "$T/choice" ] || exit 1
sed -n "$(cat "$T/choice")p" "$T/pick.in"'
fake xclip 'cat "$T/targets"'
fake xsel 'case "$*" in
    *--output*) cat "$T/clip" ;;
    *--input*)  cat > "$T/xsel.in" ;;
esac'
# clipnotify: makes the next queued copy current, or exits 1 (no X server).
fake clipnotify 'n=$(ls "$T/queue" 2>/dev/null | head -n 1)
[ -n "$n" ] || exit 1
cp "$T/queue/$n/clip" "$T/clip"; cp "$T/queue/$n/targets" "$T/targets"
rm -r "${T:?}/queue/$n"'

session() { printf 'DESKTOP_NAME=%s\n' "$1" > /etc/hadron-desktop/session; }
reset() { rm -rf "${T:?}/log" "$T/choice" "$T/wl-copy.out" "$T/xsel.in" "$T/pick.in" "$T/run/hadron-clipboard"; : > "$T/log"; }
db="$T/run/hadron-clipboard/cliphist.db"

# --- 1. dispatch -----------------------------------------------------------
session niri; reset
hadron-clipboard
expect "niri: show opens Noctalia's clipboard panel" "$(cat "$T/log")" "noctalia msg panel-toggle clipboard"
reset; hadron-clipboard daemon; rc=$?
expect "niri: daemon records nothing (Noctalia does)" "$rc:$(cat "$T/log")" "0:"
reset; hadron-clipboard clear
expect "niri: clear clears Noctalia's history" "$(cat "$T/log")" "noctalia msg clipboard-clear"

for d in Sway COSMIC; do
    session "$d"; reset
    hadron-clipboard daemon
    expect "$d: daemon feeds cliphist from wl-paste --watch" "$(cat "$T/log")" \
        "wl-paste --watch cliphist -db-path $db -max-items 200 store"
    expect "$d: the history directory is private" "$(stat -c %a "$T/run/hadron-clipboard")" 700
    reset; echo 1 > "$T/choice"; hadron-clipboard show
    expect "$d: show decodes the chosen entry onto the clipboard" "$(cat "$T/wl-copy.out")" "decoded:2	second"
    expect "$d: show lists through the picker" "$(head -n 1 "$T/pick.in")" "2	second"
    reset; hadron-clipboard
    expect "$d: a cancelled pick copies nothing" "$([ -e "$T/wl-copy.out" ] && echo copied)" ""
    reset; hadron-clipboard clear
    expect "$d: clear wipes cliphist" "$(cat "$T/log")" "cliphist -db-path $db -max-items 200 wipe"
done

session Sway; reset
HADRON_CLIPBOARD_MAX=50 hadron-clipboard daemon
expect "HADRON_CLIPBOARD_MAX sets the size" "$(cat "$T/log")" "wl-paste --watch cliphist -db-path $db -max-items 50 store"
reset; HADRON_CLIPBOARD_MAX=lots hadron-clipboard daemon
expect "a bad HADRON_CLIPBOARD_MAX falls back to 200" "$(cat "$T/log")" "wl-paste --watch cliphist -db-path $db -max-items 200 store"
reset; hadron-clipboard bogus 2>/dev/null; rc=$?
expect "an unknown command is a usage error" "$rc" 2
reset; (unset XDG_RUNTIME_DIR; hadron-clipboard daemon 2>/dev/null); rc=$?
expect "no XDG_RUNTIME_DIR: refuses, never falls back to /tmp" "$rc:$(cat "$T/log")" "1:"

# --- 2. the i3 recorder and picker ------------------------------------------
session i3; reset
# queue CLIP TARGETS...: one copy for the fake clipnotify to deliver.
q=0
queue() {
    q=$((q + 1)); e="$T/queue/$(printf '%03d' "$q")"; mkdir -p "$e"
    printf '%b' "$1" > "$e/clip"; shift
    printf '%s\n' "$@" > "$e/targets"
}
printf 'first' > "$T/clip"; printf 'TARGETS\nUTF8_STRING\n' > "$T/targets"
queue 'second' TARGETS UTF8_STRING
queue 'hunter2' TARGETS UTF8_STRING x-kde-passwordManagerHint
queue 'PNGDATA' TARGETS image/png
queue '' TARGETS UTF8_STRING
queue 'first' TARGETS STRING
queue 'third' TARGETS text/plain
queue 'four\nlines\n' TARGETS UTF8_STRING
HADRON_CLIPBOARD_MAX=3 hadron-clipboard daemon; rc=$?
expect "i3: the daemon ends when clipnotify loses the X server" "$rc" 0
h="$T/run/hadron-clipboard"
got=$(for f in "$h"/[0-9]*; do printf '%s=%s;' "${f##*/}" "$(cat "$f")"; done)
expect "i3: text only, no secret, no empty copy, repeats move up, cut to 3" "$got" \
    "00000003=first;00000004=third;00000005=four
lines;"
expect "i3: the history directory is private" "$(stat -c %a "$h")" 700
reset_keep() { rm -f "$T/choice" "$T/xsel.in" "$T/pick.in"; }
reset_keep; echo 1 > "$T/choice"; hadron-clipboard
expect "i3: the picker lists newest first, one line each" "$(cat "$T/pick.in")" "5  four lines
4  third
3  first"
if cmp -s "$T/xsel.in" "$h/00000005"; then ok "i3: the chosen entry goes back byte for byte"; else err "i3: xsel got [$(cat "$T/xsel.in")]"; fi
reset_keep; hadron-clipboard
expect "i3: a cancelled pick copies nothing" "$([ -e "$T/xsel.in" ] && echo copied)" ""
reset_keep; printf '99  gone\n' > "$T/fixed"; fake hadron-pick 'cat > /dev/null; cat "$T/fixed"'
hadron-clipboard
expect "i3: an entry that is gone copies nothing" "$([ -e "$T/xsel.in" ] && echo copied)" ""
hadron-clipboard clear
expect "i3: clear removes every entry" "$(find "$h" -name '[0-9]*' | wc -l)" 0

# --- 3. hadron-menu ---------------------------------------------------------
fake hadron-pick 'cat > "$T/pick.in"; exit 1'
session Sway; "$repo/rootfs/usr/bin/hadron-menu"
expect "hadron-menu offers Clipboard" "$(cut -f1 "$T/pick.in" | grep -c '^Clipboard$')" 1
rm "$B/hadron-clipboard"; "$repo/rootfs/usr/bin/hadron-menu"
expect "hadron-menu hides Clipboard without hadron-clipboard (agent)" "$(cut -f1 "$T/pick.in" | grep -c '^Clipboard$')" 0
expect "hadron-menu keeps the other entries" "$(cut -f1 "$T/pick.in" | tr '\n' ' ')" "Apps Wi-Fi Bluetooth Audio Capture Emoji Night light Update Help Power "

# --- 4. wiring --------------------------------------------------------------
count() { grep -v '^[[:space:]]*\(#\|//\)' "$2" | grep -ci -- "$1"; }
expect "sway starts the recorder" "$(count 'exec hadron-clipboard daemon' /src/rootfs-sway/etc/sway/config)" 1
expect "sway binds Super+Ctrl+V once" "$(count 'mod+ctrl+v' /src/rootfs-sway/etc/sway/config)" 1
expect "i3 starts the recorder" "$(cat /src/rootfs-i3/etc/i3/config.d/*.conf | grep -c '^exec --no-startup-id hadron-clipboard daemon$')" 1
expect "i3 binds Super+Ctrl+V once" "$(cat /src/rootfs-i3/etc/i3/config /src/rootfs-i3/etc/i3/config.d/*.conf | grep -v '^#' | grep -ci 'mod+ctrl+v')" 1
expect "niri binds Super+Ctrl+V once" "$(count 'Mod+Ctrl+V' /src/rootfs-niri/etc/niri/config.kdl)" 1
expect "niri keeps 200 entries in Noctalia" "$(grep -c '^clipboard_history_max_entries = 200$' /src/rootfs-niri/usr/share/hadron-desktop/noctalia-defaults.toml)" 1
expect "COSMIC starts the recorder" "$(grep -c '^Exec=hadron-clipboard daemon$' /src/rootfs-cosmic/etc/xdg/autostart/hadron-clipboard.desktop)" 1
expect "COSMIC binds Super+Ctrl+V" "$(grep -c 'key: "v"): Spawn("hadron-clipboard")' /src/Dockerfile)" 2
expect "the agent image shadows the i3 file with comments only" "$(grep -cv '^#' /src/rootfs-agent/etc/i3/config.d/40-clipboard.conf)" 0
expect "the agent image removes the scripts" "$(grep -c 'rm -f /usr/bin/hadron-clipboard /usr/bin/clipnotify' /src/Dockerfile.agent)" 1

rm -rf "$T"
[ "$fail" -eq 0 ] && echo "all clipboard tests passed"
exit "$fail"
