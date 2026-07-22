#!/usr/bin/env bash
# Assert the ISO overlay directory is staged correctly.
#
#   test/boot-ux/check-iso-overlay.sh [OVERLAY_DIR]
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
CFG="$DIR/boot/grub2/grub.cfg"
QUIET='quiet splash loglevel=3 udev.log_level=3 systemd.show_status=false rd.systemd.show_status=false'
fail=0
check() {
    local desc="$1"; shift
    if eval "$*" >/dev/null 2>&1; then echo "PASS: $desc"; else echo "FAIL: $desc"; fail=1; fi
}

# Body of the named menuentry, from its opening line to the closing brace.
entry() { awk -v pat="menuentry \"$1\"" 'index($0, pat)==1, /^}/' "$CFG"; }

check "grub.cfg staged"                 "test -f '$CFG'"
check "theme.txt staged"                "test -f '$DIR/boot/grub2/themes/hadron/theme.txt'"
check "font staged"                     "test -f '$DIR/boot/grub2/themes/hadron/unicode.pf2'"
check "background staged"               "test -f '$DIR/boot/grub2/themes/hadron/background.tga'"
check "no unrendered placeholders in grub.cfg" \
    "test -f '$CFG' && ! grep -q '{{' '$CFG'"
check "no unrendered placeholder in theme.txt" \
    "test -f '$DIR/boot/grub2/themes/hadron/theme.txt' && ! grep -q '@VARIANT_SUBTITLE@' '$DIR/boot/grub2/themes/hadron/theme.txt'"
check "menu is retitled to hadron"      "grep -q 'menuentry \"hadron-desktop\"' '$CFG'"
check "no stale Kairos titles"          "test -f '$CFG' && ! grep -q 'menuentry \"Kairos' '$CFG'"
check "default entry carries install-mode" \
    "entry 'hadron-desktop' | grep -q ' install-mode '"
check "quiet cmdline on the default entry" \
    "entry 'hadron-desktop' | grep -qF '$QUIET'"
check "nomodeset rendered on the default entry" \
    "entry 'hadron-desktop' | grep -q 'vga=795 nomodeset'"
check "debug entry left verbose" \
    "entry 'hadron-desktop (debug)' | grep -q 'rd.debug' && ! entry 'hadron-desktop (debug)' | grep -q 'quiet'"
check "recovery entry left verbose" \
    "entry 'hadron-desktop (remote recovery mode)' | grep -q 'kairos.remote_recovery_mode' && ! entry 'hadron-desktop (remote recovery mode)' | grep -q 'quiet'"
check "theme load is guarded"           "grep -q 'if \[ -f (\$root)/boot/grub2/themes/hadron/theme.txt \]' '$CFG'"
# The ISO's GRUB core has no gfxmenu built in: without these the live menu prints
# "file `/boot/grub2/x86_64-efi/gfxmenu.mod' not found" and drops to text mode.
check "gfxmenu module staged (EFI)"     "test -f '$DIR/boot/grub2/x86_64-efi/gfxmenu.mod'"
check "module dep index staged (EFI)"   "test -f '$DIR/boot/grub2/x86_64-efi/moddep.lst'"
check "gfxmenu module staged (BIOS)"    "test -f '$DIR/boot/grub2/i386-pc/gfxmenu.mod'"

exit "$fail"
