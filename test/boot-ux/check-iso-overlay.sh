#!/usr/bin/env bash
# Assert the ISO overlay directory is staged correctly.
#
#   test/boot-ux/check-iso-overlay.sh [OVERLAY_DIR] [EXPECTED_SUBTITLE]
#
# e.g. check-iso-overlay.sh build/sway-desktop/iso/iso-overlay  'sway · wayland · kairos'
#      check-iso-overlay.sh build/i3-desktop/iso/iso-overlay    'i3 · xlibre · kairos'
#      check-iso-overlay.sh build/agent-desktop/iso/iso-overlay 'i3 · xlibre · kairos'
#
# Run after auroraboot/build.sh has staged the overlay (STAGE_ONLY=1 stages and
# exits without building an ISO). The placeholder check is the important one:
# AuroraBoot does NOT expand {{NOMODESET}}/{{EXTEND_CMDLINE}} in an OVERLAID
# file, only in its own default, so an unrendered placeholder would go straight
# onto the kernel cmdline as a literal.
#
# NOTE ON NEGATIVE CHECKS: every `! grep ...` assertion is paired with an
# explicit `test -f`. grep on a missing file exits 2, which `!` would invert to
# 0 and report a bogus PASS — a vacuous test. The file guard makes a missing or
# unstaged grub.cfg fail these checks instead of silently passing them.
set -euo pipefail
DIR="${1:-build/sway-desktop/iso/iso-overlay}"
EXPECTED_SUBTITLE="${2:-sway · wayland · kairos}"
CFG="$DIR/boot/grub2/grub.cfg"
THEME="$DIR/boot/grub2/themes/isotope/theme.txt"
QUIET='quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false'
fail=0
check() {
    local desc="$1"; shift
    if eval "$*" >/dev/null 2>&1; then echo "PASS: $desc"; else echo "FAIL: $desc"; fail=1; fi
}

# Body of the named menuentry, from its opening line to the closing brace.
entry() { awk -v pat="menuentry \"$1\"" 'index($0, pat)==1, /^}/' "$CFG"; }

check "grub.cfg staged"                 "test -f '$CFG'"
check "theme.txt staged"                "test -f '$DIR/boot/grub2/themes/isotope/theme.txt'"
check "font staged"                     "test -f '$DIR/boot/grub2/themes/isotope/unicode.pf2'"
check "background staged"               "test -f '$DIR/boot/grub2/themes/isotope/background.tga'"
check "wordmark staged"                 "test -f '$DIR/boot/grub2/themes/isotope/wordmark.tga'"
check "selected-entry slices staged"    "test \$(ls '$DIR'/boot/grub2/themes/isotope/select_*.tga | wc -l) -eq 9"
check "menu fonts staged"               "test -f '$DIR/boot/grub2/themes/isotope/inter-medium-18.pf2' -a -f '$DIR/boot/grub2/themes/isotope/inter-medium-24.pf2'"
check "live menu loads the menu fonts"  "grep -q 'loadfont .*inter-medium-24.pf2' '$CFG'"
check "no unrendered placeholders in grub.cfg" \
    "test -f '$CFG' && ! grep -q '{{' '$CFG'"
check "no unrendered placeholder in theme.txt" \
    "test -f '$THEME' && ! grep -q '@VARIANT_SUBTITLE@' '$THEME'"
# Absence of the placeholder is not enough: a WRONG subtitle also has no
# placeholder. This asserts the substituted VALUE, which is what caught the live
# menu claiming "agent · xlibre · kairos" while the installed system said i3.
check "theme subtitle is '$EXPECTED_SUBTITLE'" \
    "test -f '$THEME' && grep -qF '$EXPECTED_SUBTITLE' '$THEME'"
check "menu is retitled to isotope"      "grep -q 'menuentry \"isotope\"' '$CFG'"
# One install entry (AuroraBoot#869): immucore turns install-mode-interactive
# into /run/cos/install_mode_interactive, which 90_desktop_installer.yaml hooks.
check "default names the install entry by id" \
    "grep -qx 'set default=\"isotope-install\"' '$CFG' && grep -q 'menuentry \"isotope\" --id isotope-install ' '$CFG'"
check "no stale Kairos titles"          "test -f '$CFG' && ! grep -q 'menuentry \"Kairos' '$CFG'"
check "default entry asks for the interactive installer" \
    "entry 'isotope' | grep -q ' install-mode-interactive '"
# The unattended install-mode entry, the plain "manual" boot and remote
# recovery (AuroraBoot#880) are retired upstream.
check "no plain install-mode entry"     "test -f '$CFG' && ! grep -qE ' install-mode( |\$)' '$CFG'"
check "no manual entry"                 "test -f '$CFG' && ! grep -q 'menuentry \"isotope (manual)\"' '$CFG'"
check "no remote recovery entry"        "test -f '$CFG' && ! grep -q 'remote_recovery' '$CFG'"
check "quiet cmdline on the default entry" \
    "entry 'isotope' | grep -qF '$QUIET'"
check "console palette on the default entry" \
    "grep -q '^set isotope_vt_palette=\"vt.default_red=' '$CFG' && entry 'isotope' | grep -qF ' \$isotope_vt_palette'"
# The normal entries load the graphics driver and its firmware; "safe
# graphics" is the same install with nomodeset, for machines where that driver
# hangs or shows nothing.
check "no nomodeset on the default entry" \
    "test -f '$CFG' && ! entry 'isotope' | grep -q nomodeset"
# The desktop ISOs start the graphical installer; this entry keeps the text
# one (isotope-install-ui reads isotope.installer=text).
check "text installer entry"            "entry 'isotope (text installer)' | grep -q ' install-mode-interactive isotope.installer=text '"
check "only the text entry asks for text" \
    "test -f '$CFG' && [ \"\$(grep -c '^[^#]*isotope.installer=text' '$CFG')\" -eq 1 ]"
check "safe graphics entry installs with nomodeset" \
    "entry 'isotope (safe graphics)' | grep -q 'vga=795 nomodeset install-mode-interactive '"
check "debug entry left verbose" \
    "entry 'isotope (debug)' | grep -q 'rd.debug' && ! entry 'isotope (debug)' | grep -q 'quiet'"
# Boot details: the install entry plus isotope.bootvine, which the splash
# must see (so `splash` too). bootvine quiets the console, so not on debug.
check "boot details entry starts bootvine from the splash" \
    "entry 'isotope (boot details)' | grep -q ' quiet splash ' && entry 'isotope (boot details)' | grep -q ' isotope.bootvine '"
# Kernel errors reach the console only on boot details (isotope-console-level).
check "boot details entry passes loglevel=4, the install entry loglevel=3" \
    "entry 'isotope (boot details)' | grep -q ' loglevel=4 ' && ! entry 'isotope (boot details)' | grep -q ' loglevel=3 ' && entry 'isotope' | grep -q ' loglevel=3 '"
check "debug entry has no bootvine" \
    "test -f '$CFG' && ! entry 'isotope (debug)' | grep -q bootvine"
# Hadron ships dracut 110 or newer, which renamed rd.live.overlay.overlayfs to
# rd.overlay and logs a deprecation warning for the old name.
check "every live entry asks for the overlay as rd.overlay" \
    "test -f '$CFG' && [ \"\$(grep -c '^ *linux ' '$CFG')\" -gt 0 ] && [ \"\$(grep -c '^ *linux ' '$CFG')\" -eq \"\$(grep -cE '^ *linux .* rd\.overlay( |\$)' '$CFG')\" ]"
check "no deprecated rd.live.overlay.overlayfs" \
    "test -f '$CFG' && ! grep -q 'rd\.live\.overlay\.overlayfs' '$CFG'"
# GRUB has no hiddenentry command: on EFI it printed "can't find command".
check "no hiddenentry"                  "test -f '$CFG' && ! grep -q hiddenentry '$CFG'"
# Each echo opens a text box over the theme.
check "no echo in the menu entries"     "test -f '$CFG' && ! grep -q '^[^#]*echo Loading' '$CFG'"
# Unquoted, an empty model makes the Thor test true on every machine.
check "Thor test is quoted"             "grep -qF 'if [ \"\${model}\" = \"Thor\" ]' '$CFG'"
check "smbios runs only on arm64"       "grep -B1 '^ *smbios ' '$CFG' | grep -qF '\${grub_cpu}\" = \"arm64\"'"
check "theme load is guarded"           "grep -q 'if \[ -f (\$root)/boot/grub2/themes/isotope/theme.txt \]' '$CFG'"
# The ISO's GRUB core has no gfxmenu built in: without these the live menu prints
# "file `/boot/grub2/x86_64-efi/gfxmenu.mod' not found" and drops to text mode.
check "gfxmenu module staged (EFI)"     "test -f '$DIR/boot/grub2/x86_64-efi/gfxmenu.mod'"
check "module dep index staged (EFI)"   "test -f '$DIR/boot/grub2/x86_64-efi/moddep.lst'"
check "gfxmenu module staged (BIOS)"    "test -f '$DIR/boot/grub2/i386-pc/gfxmenu.mod'"

exit "$fail"
