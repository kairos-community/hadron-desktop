#!/usr/bin/env bash
# Assert the Isotope look is in a built image.
#
#   test/look/check-look.sh IMAGE DESKTOP
#
# e.g. test/look/check-look.sh sway-desktop:dev sway
#      test/look/check-look.sh cosmic-desktop:dev cosmic
#
# Prints PASS:/FAIL: lines and exits 1 on any FAIL. DESKTOP is sway, i3, niri
# or cosmic; checks for one variant are guarded by it.
set -euo pipefail
IMAGE="${1:-sway-desktop:dev}"
DESKTOP="${2:-sway}"
fail=0
check() {
    local desc="$1"; shift
    if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"; fail=1
    fi
}

# --- Icons: Conflux on every variant ---------------------------------------
C=/usr/share/icons/Conflux
check "Conflux has an index.theme"            "test -f $C/index.theme"
check "Conflux inherits Cosmic then hicolor"  "grep -qx 'Inherits=Cosmic,hicolor' $C/index.theme"
check "every inherited theme is installed" \
    "test -f $C/index.theme && for t in \$(sed -n 's/^Inherits=//p' $C/index.theme | tr ',' ' '); do test -f /usr/share/icons/\$t/index.theme || exit 1; done"
# With -L, -type l matches only links that do not resolve (BusyBox find has no -xtype).
check "every Conflux link resolves"           "test -d $C && test -z \"\$(find -L $C -type l)\""

# Conflux is GPL-3.0: ship its licence and where it came from, and only the
# theme itself, not the upstream repo's build files and previews.
L=/usr/share/licenses/conflux
check "Conflux's GPL-3.0 LICENSE is installed" \
    "grep -q 'GNU GENERAL PUBLIC LICENSE' $L/LICENSE && test -s $L/CREDITS.md"
# The pin comes from the Dockerfile, so the check follows a bump.
pin=$(sed -n 's/^ARG CONFLUX_COMMIT=//p' "$(dirname "$0")/../../Dockerfile")
if [ -z "$pin" ]; then echo "FAIL: no CONFLUX_COMMIT in the Dockerfile"; fail=1; else
    check "Conflux's SOURCE names the pinned commit $pin" "grep -q '$pin' $L/SOURCE"
fi
check "Conflux holds only the theme (no Workspace, images, preview.png, update.sh, .github)" \
    "for n in Workspace images preview.png update.sh .github README.md; do test ! -e $C/\$n || exit 1; done"
check "the Conflux alias links are listed in conflux-links.txt" "test -s /usr/share/isotope/conflux-links.txt"

# An app ID resolves if Conflux has apps/scalable/<id>.svg, or it is listed in
# conflux-missing.txt (an app with no fitting Conflux icon).
# shellcheck disable=SC2016 # expanded later, inside the image
resolve='
id_ok() {
    test -e /usr/share/icons/Conflux/apps/scalable/$1.svg && return 0
    grep -q "^$1 " /usr/share/isotope/conflux-missing.txt
}'
check "first-run apps have a Conflux icon or are listed as missing" \
    "$resolve; test -f /usr/share/isotope/first-run-apps.list && grep -v '^#' /usr/share/isotope/first-run-apps.list | cut -d'|' -f2 | while read -r id; do [ -n \"\$id\" ] || continue; id_ok \$id || { echo \$id; exit 1; }; done"
if [ "$DESKTOP" = cosmic ]; then
    F=/usr/share/cosmic/com.system76.CosmicAppList/v1/favorites
    check "COSMIC favorites have a Conflux icon or are listed as missing" \
        "$resolve; test -f $F && grep -o '\"[^\"]*\"' $F | tr -d '\"' | while read -r id; do id_ok \$id || { echo \$id; exit 1; }; done"
fi

check "gsettings icon-theme is Conflux" \
    "test \"\$(gsettings get org.gnome.desktop.interface icon-theme)\" = \"'Conflux'\""
# GTK's window buttons go on the left on COSMIC only (its own schema override),
# as its patched libcosmic does; the tiling variants keep GTK's default.
if [ "$DESKTOP" != cosmic ]; then
    check "gsettings button-layout is not COSMIC's left layout" \
        "v=\$(gsettings get org.gnome.desktop.wm.preferences button-layout) && test -n \"\$v\" && test \"\$v\" != \"'close,minimize,maximize:'\""
fi
check "fuzzel uses Conflux" "grep -qx 'icon-theme=Conflux' /etc/xdg/fuzzel/fuzzel.ini"
if [ "$DESKTOP" = i3 ]; then
    check "dunst uses Conflux" "grep -qx '[[:space:]]*icon_theme = Conflux' /etc/dunst/dunstrc"
    check "dunst looks icons up recursively" \
        "grep -qx '[[:space:]]*enable_recursive_icon_lookup = true' /etc/dunst/dunstrc"
fi
if [ "$DESKTOP" = sway ]; then
    # mako globs <icon-path>/*/<number>/<name>.*, so test that glob for an app
    # icon (a link) and a status icon, with the icon-path from its own config.
    check "mako finds an app icon and a status icon in its icon-path" \
        "p=\$(sed -n 's/^icon-path=//p' /etc/mako/config | tr ':' ' '); test -n \"\$p\"; for n in org.videolan.VLC protonvpn-tray; do f=; for d in \$p; do for x in \$d/*/[0-9]*/\$n.*; do test -e \"\$x\" && f=\$x; done; done; test -n \"\$f\" || exit 1; done"
fi

# --- COSMIC theme: the Isotope theme as COSMIC's system defaults ------------
if [ "$DESKTOP" = cosmic ]; then
    T=/usr/share/cosmic/com.system76.CosmicTheme
    check "COSMIC theme dirs exist (dark, light, builders)" \
        "for d in Dark Light Dark.Builder Light.Builder; do test -s $T.\$d/v2/accent || exit 1; done"
    check "COSMIC defaults to dark" "test \"\$(cat $T.Mode/v1/is_dark)\" = true"
    # The night accent of theme/palette.toml, as the hex COSMIC's config writes
    # (RON: Some("#E9E7C1FF")). Compared case-insensitively.
    accent_of() { (cd "$(dirname "$0")/../.." && awk -v sec="[$1]" '$0==sec{n=1;next} /^\[/{n=0} n && $1=="accent"{gsub(/"/,"",$3); print toupper(substr($3,2,6)); exit}' theme/palette.toml); }
    for m in night:Dark day:Light; do
        want=$(accent_of "${m%%:*}"); M=${m##*:}
        if [ -z "$want" ]; then echo "FAIL: no ${m%%:*} accent in theme/palette.toml"; fail=1; continue; fi
        check "COSMIC $M builder accent is the Isotope accent #$want" \
            "grep -qi '#${want}ff' $T.$M.Builder/v2/accent"
        check "COSMIC $M theme accent is the Isotope accent #$want" \
            "grep -qi 'base: \"#${want}ff\"' $T.$M/v2/accent"
    done
    # The focused window's outline: 1 or 2 px, in the palette's dim (a neutral).
    # The builder keeps it as hex; the computed theme as RGB floats, so there
    # only its presence is checked.
    hint_of() { (cd "$(dirname "$0")/../.." && python3 -c 'import json,sys; m=json.load(open("theme/cosmic-theme.json"))[sys.argv[1]]; print(m["builder"]["window_hint"][1:].upper(), m["look"]["active_hint"])' "$1"); }
    for M in Dark Light; do
        m=$(echo "$M" | tr '[:upper:]' '[:lower:]')
        read -r hint width <<EOF_HINT
$(hint_of "$m")
EOF_HINT
        if [ -z "$hint" ]; then echo "FAIL: no $m window_hint in theme/cosmic-theme.json"; fail=1; continue; fi
        check "COSMIC $M active hint is ${width} px in the neutral #$hint" \
            "grep -qx '$width' $T.$M.Builder/v2/active_hint && grep -qi '#${hint}ff' $T.$M.Builder/v2/window_hint && grep -qx '$width' $T.$M/v2/active_hint && grep -q '^Some(' $T.$M/v2/window_hint"
    done
fi

# --- COSMIC toolkit, compositor, panels and the Isotope layout -------------
if [ "$DESKTOP" = cosmic ]; then
    K=/usr/share/cosmic/com.system76.CosmicTk/v1
    check "CosmicTk icon_theme is Conflux"     "grep -qx '\"Conflux\"' $K/icon_theme"
    check "CosmicTk interface_font is Inter Variable" "grep -q 'family: \"Inter Variable\"' $K/interface_font"
    check "CosmicTk monospace_font is JetBrains Mono" "grep -q 'family: \"JetBrains Mono\"' $K/monospace_font"
    check "CosmicTk header_size and interface_density are Standard" \
        "grep -qx Standard $K/header_size && grep -qx Standard $K/interface_density"
    # Window buttons on the left: every COSMIC binary that reads CosmicTk must
    # be built with the patched libcosmic (patches/cosmic/libcosmic), whose
    # CosmicTk has the window_controls_left key; at least one must read it.
    check "every ELF that reads CosmicTk knows window_controls_left (patched libcosmic)" \
        "n=0; for f in \$(find /usr/bin /usr/libexec -type f 2>/dev/null); do head -c4 \$f | grep -q ELF || continue; grep -qa com.system76.CosmicTk \$f || continue; n=\$((n+1)); grep -qa window_controls_left \$f || { echo \$f; exit 1; }; done; [ \$n -gt 0 ]"
    check "CosmicTk window_controls_left is true" "grep -qx true $K/window_controls_left"
    check "gsettings button-layout default (compiled) is the left layout; not proof that apps follow it" \
        "test \"\$(gsettings get org.gnome.desktop.wm.preferences button-layout)\" = \"'close,minimize,maximize:'\""
    P=/usr/share/cosmic/com.system76.CosmicComp/v1
    # The spec's fallback for tiling: a thin outline on the focused window, in
    # the palette's neutral dim, not the accent (tools/gen-theme.py).
    check "CosmicComp active_hint is true"     "grep -qx true $P/active_hint"
    check "CosmicComp shadows tiled windows"   "grep -q 'shadow_tiled_windows: true' $P/appearance_settings"
    L=/usr/share/cosmic-layouts/isotope
    check "the wallpaper follows light and dark (autostart entry, COSMIC only)" \
        "grep -qx 'Exec=isotope wallpaper-follow' /etc/xdg/autostart/isotope-wallpaper.desktop && grep -qx 'OnlyShowIn=COSMIC;' /etc/xdg/autostart/isotope-wallpaper.desktop && isotope help | grep -q wallpaper-follow"
    check "the Isotope layout has an icon and a name" "test -s $L/icon.png && grep -q '^name en=\"Isotope\"' $L/layout.kdl"
    check "the system panel defaults are the Isotope layout's (no drift)" \
        "set -- $L/com.system76.CosmicPanel*; test -d \$1 && for d in \"\$@\"; do o=/usr/share/cosmic/\$(basename \$d); test -d \$o || exit 1; a=\$(cd \$d && find . -type f -exec sha256sum {} + | sort); b=\$(cd \$o && find . -type f -exec sha256sum {} + | sort); test -n \"\$a\" && test \"\$a\" = \"\$b\" || exit 1; done"
    check "the Isotope layout is listed first (lowest id)" \
        "own=\$(sed -n 's/^id //p' $L/layout.kdl); test -n \"\$own\"; for f in /usr/share/cosmic-layouts/*/layout.kdl; do [ \$f = $L/layout.kdl ] && continue; o=\$(sed -n 's/^id //p' \$f); [ \"\$own\" -lt \"\$o\" ] || exit 1; done; test \$(ls -d /usr/share/cosmic-layouts/*/ | wc -l) -gt 1"
fi

# --- COSMIC top bar: the Isotope mark, the focused app, quick settings (applets/)
if [ "$DESKTOP" = cosmic ]; then
    B=/usr/bin/isotope-applet-mark
    check "isotope-applet-mark is installed and links (ldd clean)" \
        "test -x $B && ldd $B >/dev/null && ! ldd $B 2>&1 | grep -q 'not found'"
    check "isotope-applet-mark is built with the patched libcosmic (window_controls_left)" \
        "grep -qa window_controls_left $B"
    check "the mark's icon is a symbolic icon in hicolor" \
        "grep -q '<svg' /usr/share/icons/hicolor/scalable/apps/io.kairos.Isotope.Mark-symbolic.svg"
    check "the mark's applet entry runs isotope-applet-mark, a panel applet with the host display" \
        "D=/usr/share/applications/io.kairos.Isotope.Mark.desktop; grep -qx 'Exec=isotope-applet-mark' \$D && grep -qx 'X-CosmicApplet=true' \$D && grep -qx 'X-HostWaylandDisplay=true' \$D"
    check "the mark opens isotope-menu, which is on the image" \
        "grep -qa isotope-menu $B && test -x /usr/bin/isotope-menu"
    check "the top bar starts with the mark, then the workspaces" \
        "grep -q '^Some((\[\"io.kairos.Isotope.Mark\", \"com.system76.CosmicAppletWorkspaces\"\]' /usr/share/cosmic/com.system76.CosmicPanel.Panel/v1/plugins_wings"
    # The workspaces themselves (cosmic-applet-workspaces' numbered
    # indicators), not the stock "Workspaces" button that opens the overview.
    check "the top bar has no Workspaces button" \
        "! grep -q CosmicPanelWorkspacesButton /usr/share/cosmic/com.system76.CosmicPanel.Panel/v1/plugins_wings"
    # The quick settings (applets/isotope-quick) in place of the stock
    # network, Bluetooth, audio and power applets; battery and time stay.
    Q=/usr/bin/isotope-applet-quick
    check "isotope-applet-quick is installed and links (ldd clean)" \
        "test -x $Q && ldd $Q >/dev/null && ! ldd $Q 2>&1 | grep -q 'not found'"
    check "isotope-applet-quick is built with the patched libcosmic (window_controls_left)" \
        "grep -qa window_controls_left $Q"
    check "the quick settings' icon is a symbolic icon in hicolor" \
        "grep -q '<svg' /usr/share/icons/hicolor/scalable/apps/io.kairos.Isotope.QuickSettings-symbolic.svg"
    check "the quick settings' applet entry runs isotope-applet-quick, a panel applet" \
        "D=/usr/share/applications/io.kairos.Isotope.QuickSettings.desktop; grep -qx 'Exec=isotope-applet-quick' \$D && grep -qx 'X-CosmicApplet=true' \$D"
    check "the quick settings open COSMIC Settings' pages and cosmic-osd, which are on the image" \
        "grep -qa wireless $Q && grep -qa cosmic-osd $Q && test -x /usr/bin/cosmic-settings && test -x /usr/bin/cosmic-osd"
    check "the top bar's right side is status area, notifications, quick settings, battery, time" \
        "grep -qF '[\"com.system76.CosmicAppletStatusArea\", \"com.system76.CosmicAppletNotifications\", \"io.kairos.Isotope.QuickSettings\", \"com.system76.CosmicAppletBattery\", \"com.system76.CosmicAppletTime\"]' /usr/share/cosmic/com.system76.CosmicPanel.Panel/v1/plugins_wings"
    # The stock applets the quick settings replace stay installed, so a user
    # can add them back.
    for id in Network Bluetooth Audio Power; do
        check "the stock com.system76.CosmicApplet$id stays installed" \
            "test -f /usr/share/applications/com.system76.CosmicApplet$id.desktop"
    done
    # cosmic-panel starts an applet only if it finds <id>.desktop; a missing one
    # leaves a silent gap in the bar or the dock.
    check "every applet in the Panel and Dock plugins_* has a .desktop file" \
        "set -- /usr/share/cosmic/com.system76.CosmicPanel.Panel/v1/plugins_* /usr/share/cosmic/com.system76.CosmicPanel.Dock/v1/plugins_*; test -f \$1 && n=0 && for id in \$(cat \"\$@\" | grep -o '\"[^\"]*\"' | tr -d '\"'); do n=\$((n+1)); test -f /usr/share/applications/\$id.desktop || { echo \$id; exit 1; }; done; [ \$n -gt 0 ]"
fi

# --- COSMIC login screen: the centred, branded cosmic-greeter --------------
if [ "$DESKTOP" = cosmic ]; then
    W=/usr/share/backgrounds/isotope
    # The soft wallpapers the patched greeter shows (tools/gen-wallpaper.py --blur).
    for m in night day; do
        check "the login screen's soft $m wallpaper is a PNG ($W/isotope-desktop-$m-blur.png)" \
            "test \"\$(head -c 4 $W/isotope-desktop-$m-blur.png | tail -c 3)\" = PNG"
    done
    # The mark: the night brand under a dark theme, the day palette's mark
    # (ink shell, deep brand core) under a light one.
    role_of() { (cd "$(dirname "$0")/../.." && awk -v sec="[$1]" -v r="$2" '$0==sec{n=1;next} /^\[/{n=0} n && $1==r{gsub(/"/,"",$3); print $3; exit}' theme/palette.toml); }
    for f in greeter-mark.svg:night:brand greeter-mark-day.svg:day:text greeter-mark-day.svg:day:brand_deep; do
        IFS=: read -r file mode role <<<"$f"
        want=$(role_of "$mode" "$role")
        if [ -z "$want" ]; then echo "FAIL: no $mode $role in theme/palette.toml"; fail=1; continue; fi
        check "the login screen's $file has the $mode $role $want" \
            "grep -q '<svg' /usr/share/isotope/$file && grep -qi 'fill=\"$want\"' /usr/share/isotope/$file"
    done
    # The greeter is built with patches/cosmic/cosmic-greeter: it names both
    # marks and the soft wallpapers.
    check "cosmic-greeter is the patched, centred one (names both marks and -blur.png)" \
        "grep -qa greeter-mark.svg /usr/bin/cosmic-greeter && grep -qa greeter-mark-day.svg /usr/bin/cosmic-greeter && grep -qa -- -blur.png /usr/bin/cosmic-greeter"
    # A user with no wallpaper of their own (never logged in) and the screen
    # before a user is chosen get the system default, not COSMIC's nebula:
    # the greeter reads cosmic-bg's system "all" entry, which names a picture
    # the image has.
    check "cosmic-greeter falls back to the system default wallpaper, which exists" \
        "grep -qa /usr/share/cosmic/com.system76.CosmicBackground/v1/all /usr/bin/cosmic-greeter && p=\$(sed -n 's/.*source: Path(\"\\(.*\\)\").*/\\1/p' /usr/share/cosmic/com.system76.CosmicBackground/v1/all) && test -s \"\$p\" && test -s \"\${p%.png}-blur.png\""
fi

# --- The palette reaches every themed file (repository check) --------------
if (cd "$(dirname "$0")/../.." && python3 tools/gen-theme.py --check >/dev/null); then
    echo "PASS: tools/gen-theme.py --check (the greeter mark and every config match the palette)"
else
    echo "FAIL: tools/gen-theme.py --check"; fail=1
fi

exit $fail
