#!/usr/bin/env python3
"""Write the Hadron palette into every config that has colours.

theme/palette.toml names each colour by its role (base, text, accent, dim...).
The configs the programs read (sway, i3, waybar, foot, fuzzel, mako, dunst,
dmenu, niri, st, Noctalia, the update tray icon, the GRUB theme, the boot
splash, the console palette on the kernel command line) stay plain files in
the tree; this script only
rewrites the colour values in them, so they never drift from the palette and a
review sees ordinary config diffs. Stdlib only. Run from anywhere:

    python3 tools/gen-theme.py           # rewrite the configs
    python3 tools/gen-theme.py --check   # exit 1 if a config drifts (CI)

How a colour is found: RULES below lists, per file, a line pattern with one
{hex} slot per colour and the role that fills it: a string for every match, a
list for one role per matching line in file order, a tuple for one role per
slot of a line. A pattern that matches nothing, or a list whose length is not
the number of matching lines, is an error,
so a renamed key cannot silently stop being themed. The VT_PALETTE files hold
a generated value instead: the kernel's vt.default_red/grn/blu lists, which
must be present. In files marked strict, a colour that no rule claims is an
error too: add a rule for it (or use a role name, as waybar's @define-color
does) instead of a literal hex.

--check also enforces the contrast floors in CONTRAST, so a palette edit cannot
make text unreadable.
"""
import json
import re
import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PALETTE = ROOT / "theme" / "palette.toml"

HEX = r"([0-9a-fA-F]{6})"
# Literal colours a strict file may not hold unclaimed. `ini` adds the bare
# `key=1a1b26` form foot and fuzzel use.
LITERAL = {
    "hash": re.compile(r"#[0-9a-fA-F]{6}\b"),
    "ini": re.compile(r"(?<![\w#])[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?\b"),
}
COMMENT = {
    "hash": re.compile(r"^\s*#(?![0-9a-fA-F]{6}\b)"),
    "css": re.compile(r"^\s*(/\*|\*)"),
    "kdl": re.compile(r"^\s*//"),
}

# The sway and i3 variables keep their historical names; the role is what counts.
WM_VARS = [
    (r"^set \$bg\s+#{hex}$", "base"),
    (r"^set \$bg_alt\s+#{hex}$", "surface"),
    (r"^set \$fg\s+#{hex}$", "text"),
    (r"^set \$blue\s+#{hex}$", "accent"),
    (r"^set \$cyan\s+#{hex}$", "glow"),
    (r"^set \$magenta\s+#{hex}$", "violet"),
    (r"^set \$red\s+#{hex}$", "error"),
    (r"^set \$dim\s+#{hex}$", "dim"),
    (r"^set \$border\s+#{hex}$", "border"),
]
# dmenu: normal background/foreground, selected background/foreground.
DMENU = (r"""-nb ["']#{hex}["'] -nf ["']#{hex}["'] -sb ["']#{hex}["'] -sf ["']#{hex}["']""",
         ("base", "text", "accent", "on_accent"))
FOOT = [
    (r"^color={hex} {hex}$", ("base", "text")),  # cursor: text under it, cursor
    (r"^background={hex}$", "base"),
    (r"^foreground={hex}$", "text"),
] + [(rf"^{kind}{i}={{hex}}$", role)
     for kind, black, white in (("regular", "crust", "subtext"), ("bright", "border", "text"))
     for i, role in enumerate([black, "error", "success", "warning", "accent", "violet", "glow", white])]

# file -> (literal kind or None for "not strict", comment kind, rules)
RULES = {
    "rootfs-sway/etc/sway/config": ("hash", "hash", WM_VARS),
    "rootfs-i3/etc/i3/config": ("hash", "hash", WM_VARS + [
        DMENU,
        (r"^exec_always --no-startup-id hadron-xroot '#{hex}' /usr/share/backgrounds/hadron/[\w.-]+\.png$", "base"),
        (r"^set \$lock i3lock .*--color={hex}$", "base"),
    ]),
    "rootfs-i3/usr/bin/hadron-screenshot": ("hash", "hash", [
        (r"hacksaw -c '#{hex}'", "accent"),
    ]),
    # The i3 menus pick through hadron-pick; strict, so no colour comes back.
    "rootfs-i3/usr/bin/hadron-audio-menu": ("hash", "hash", []),
    "rootfs-i3/usr/bin/hadron-power-menu": ("hash", "hash", [
        (r"^\s+Lock\)\s+i3lock .*--color={hex} ;;$", "base"),
    ]),
    "rootfs-i3/usr/bin/hadron-wifi-menu": ("hash", "hash", []),
    "rootfs/usr/bin/hadron-pick": ("hash", "hash", [DMENU]),
    "rootfs-sway/etc/xdg/waybar/style.css": ("hash", "css", [
        (r"^@define-color (?P<role>\w+)\s+#{hex};$", None),  # role = the name
    ]),
    "rootfs/etc/xdg/fuzzel/fuzzel.ini": ("ini", "hash", [
        (r"^background={hex}[0-9a-fA-F]{{2}}$", "base"),
        (r"^text={hex}ff$", "text"),
        (r"^prompt={hex}ff$", "glow"),
        (r"^match={hex}ff$", "accent"),
        (r"^selection={hex}ff$", "overlay"),
        (r"^selection-text={hex}ff$", "text"),
        (r"^selection-match={hex}ff$", "violet"),
        (r"^border={hex}ff$", "accent"),
    ]),
    "rootfs-sway/etc/mako/config": ("hash", "hash", [
        (r"^background-color=#{hex}[0-9a-fA-F]{{2}}$", "base"),
        (r"^text-color=#{hex}$", "text"),
        (r"^border-color=#{hex}$", ["accent", "error"]),  # normal, urgency=high
        (r"^progress-color=over #{hex}55$", "accent"),  # a third, text stays 5.6:1
    ]),
    "rootfs-sway/etc/xdg/foot/foot.ini": ("ini", "hash", FOOT),
    "rootfs-sway/etc/swaylock/config": ("ini", "hash", [
        (r"^color={hex}$", "base"),
        (r"^inside(?:-clear|-caps-lock|-ver|-wrong)?-color={hex}$", "base"),
        (r"^layout-bg-color={hex}$", "base"),
        (r"^ring-color={hex}$", "border"),
        (r"^(?:ring|text)-clear-color={hex}$", "glow"),
        (r"^(?:ring-caps-lock|text-caps-lock|caps-lock-key-hl)-color={hex}$", "warning"),
        (r"^(?:ring-ver|text-ver|key-hl)-color={hex}$", "accent"),
        (r"^(?:ring-wrong|text-wrong|bs-hl|caps-lock-bs-hl)-color={hex}$", "error"),
        (r"^(?:layout-)?text-color={hex}$", "text"),
    ]),
    "rootfs-niri/etc/xdg/foot/foot.ini": ("ini", "hash", FOOT),
    "rootfs-i3/etc/dunst/dunstrc": ("hash", "hash", [
        (r"^\s+frame_color = \"#{hex}\"$", ["accent", "error"]),  # global, critical
        (r"^\s+background = \"#{hex}\"$", "base"),
        (r"^\s+foreground = \"#{hex}\"$", ["subtext", "text", "error"]),  # low, normal, critical
        # The progress bar fill (the volume and brightness pop-up).
        (r"^\s+highlight = \"#{hex}\"$", ["accent", "accent", "error"]),  # low, normal, critical
    ]),
    "rootfs-niri/etc/niri/config.kdl": ("hash", "kdl", [
        (r"^\s+active-color \"#{hex}\"$", "accent"),     # focus ring, border
        (r"^\s+inactive-color \"#{hex}\"$", "border"),
        (r"^\s+urgent-color \"#{hex}\"$", "error"),
    ]),
    # GRUB menu theme (live and installed). The footer and countdown are text
    # to read, so dim; the subtitle under the wordmark is decoration, so muted.
    "rootfs/etc/kairos/branding/hadron-theme/theme.txt": ("hash", "hash", [
        (r'^\s+color\s+= "#{hex}"$', ["accent", "muted", "dim"]),  # wordmark, subtitle, footer
        (r'^\s+item_color\s+= "#{hex}"$', "subtext"),
        (r'^\s+selected_item_color\s+= "#{hex}"$', "glow"),
        (r'^\s+text_color\s+= "#{hex}"$', "dim"),
        (r'^\s+fg_color\s+= "#{hex}"$', "accent"),
        (r'^\s+bg_color\s+= "#{hex}"$', "base"),
        (r'^\s+border_color\s+= "#{hex}"$', "border"),
    ]),
    # The boot splash's framebuffer colours (its text-mode fallback uses the
    # console palette below by index).
    "splash/main.c": (None, "hash", [
        (r"^#define C_BG_TOP\s+0x{hex}$", "base"),
        (r"^#define C_BG_BOT\s+0x{hex}$", "crust"),
        (r"^#define C_(?:GLOW|BODY)\s+0x{hex}$", "accent"),
        (r"^#define C_CYAN\s+0x{hex}$", "glow"),
        (r"^#define C_HOT\s+0x{hex}$", "text"),
        (r"^#define C_SHADOW\s+0x{hex}$", "border"),
        (r"^#define C_DIM\s+0x{hex}$", "muted"),
        (r"^#define C_TEXT\s+0x{hex}$", "subtext"),
        # The disk passphrase panel.
        (r"^#define C_PANEL\s+0x{hex}$", "surface"),
        (r"^#define C_FIELD\s+0x{hex}$", "overlay"),
        (r"^#define C_LABEL\s+0x{hex}$", "dim"),
        (r"^#define C_ERROR\s+0x{hex}$", "error"),
    ]),
    # st: the sed that sets its default colours (the rest of the Dockerfile
    # is full of digests, so it is not strict).
    "Dockerfile": (None, "hash", [
        (r"""^\s+-e 's\|"gray90", /\\\* default foreground colour \\\*/\|"#{hex}", """, "text"),
        (r"""^\s+-e 's\|"black", /\\\* default background colour \\\*/\|"#{hex}", """, "base"),
    ]),
    # The update tray icon: a disc per daemon state, the glyph in on_accent.
    "update/icons.go": (None, "kdl", [
        (r'^\t"idle":\s+rgb\(0x{hex}\),$', "accent"),
        (r'^\t"update":\s+rgb\(0x{hex}\),$', "success"),
        (r'^\t"busy":\s+rgb\(0x{hex}\),$', "warning"),
        (r'^\t"reboot":\s+rgb\(0x{hex}\),$', "violet"),
        (r'^\t"error":\s+rgb\(0x{hex}\),$', "error"),
        (r"^var glyphColor = rgb\(0x{hex}\)$", "on_accent"),
    ]),
}

NOCTALIA = "rootfs-niri/usr/share/hadron-desktop/noctalia-palettes/Hadron.json"

# The Linux console's 16 colours, set from the kernel command line so the boot
# messages, ly and the installer sit on the desktop's navy instead of black.
# It is the terminal palette (FOOT above) with two changes for the console:
# colour 0 is also the console's background, so it is base, not crust; and
# colour 8 is the installer's and systemd's grey text, so it is dim, not the
# border grey (2:1 on base).
VT_ROLES = (["base", "error", "success", "warning", "accent", "violet", "glow", "subtext"]
            + ["dim", "error", "success", "warning", "accent", "violet", "glow", "text"])
VT_PARAMS = re.compile(r"vt\.default_red=\S+ vt\.default_grn=\S+ vt\.default_blu=[0-9a-fx,]+")
VT_PALETTE = ["rootfs/etc/kairos/branding/grubmenu.cfg", "auroraboot/live-grub.cfg.tmpl"]

# (foreground roles, background roles, minimum ratio), checked in both modes.
# dim is for inactive labels: below text contrast on purpose, but at least the
# 3:1 WCAG asks of UI components (muted, at 2.8:1, is decoration only).
CONTRAST = [
    (["text"], ["base", "surface", "overlay"], 4.5),
    (["subtext"], ["base", "surface"], 4.5),
    (["accent", "glow", "violet", "success", "warning", "error"], ["base"], 4.5),
    (["dim"], ["base", "surface"], 3.0),
    (["on_accent"], ["accent", "glow", "violet", "error", "warning", "success"], 4.5),
]


def load_palette():
    with open(PALETTE, "rb") as f:
        pal = tomllib.load(f)
    roles = set(pal["night"])
    if set(pal["day"]) != roles:
        sys.exit(f"{PALETTE}: night and day must name the same roles")
    for mode in pal.values():
        for role, value in mode.items():
            if not re.fullmatch(r"#[0-9a-f]{6}", value):
                sys.exit(f"{PALETTE}: {role} = {value!r} is not a lowercase #rrggbb")
    return pal


def theme_file(rel, text, night):
    """Return (new text, errors) with every claimed colour set from `night`."""
    literal, comment, rules = RULES[rel]
    lines = text.split("\n")
    claimed = set()  # (line number, column) of every colour a rule owns
    errors = []
    for pattern, roles in rules:
        rx = re.compile(pattern.format(hex=HEX))
        hits = [(i, m) for i, line in enumerate(lines) if (m := rx.search(line))]
        want = len(roles) if isinstance(roles, list) else None
        if not hits or (want is not None and len(hits) != want):
            errors.append(f"{rel}: /{pattern}/ matched {len(hits)} lines, expected {want or 'at least 1'}")
            continue
        for n, (i, m) in enumerate(hits):
            groups = [g for g in range(1, (m.re.groups or 0) + 1)
                      if g != m.re.groupindex.get("role")]
            if roles is None:
                role = [m.group("role")] * len(groups)
            elif isinstance(roles, tuple):
                role = list(roles)
            elif isinstance(roles, list):
                role = [roles[n]] * len(groups)
            else:
                role = [roles] * len(groups)
            if len(role) != len(groups):
                errors.append(f"{rel}:{i + 1}: {len(groups)} colours but roles {role}")
                continue
            line = lines[i]
            for g, r in sorted(zip(groups, role), reverse=True):  # right to left
                if r not in night:
                    errors.append(f"{rel}:{i + 1}: unknown role {r!r}")
                    continue
                line = line[:m.start(g)] + night[r][1:] + line[m.end(g):]
                claimed.add((i, m.start(g)))
            lines[i] = line
    if literal:
        for i, line in enumerate(lines):
            if COMMENT[comment].match(line):
                continue
            for m in LITERAL[literal].finditer(line):
                if m.group() == "00000000":  # fully transparent, not a colour
                    continue
                col = m.start() + (1 if m.group().startswith("#") else 0)
                if (i, col) not in claimed:
                    errors.append(f"{rel}:{i + 1}: colour {m.group()} has no role; add a rule in tools/gen-theme.py")
    return "\n".join(lines), errors


def vt_params(night):
    """vt.default_red=... grn ... blu, one comma list of 16 per channel."""
    chans = [[night[r][i:i + 2] for r in VT_ROLES] for i in (1, 3, 5)]
    return " ".join(f"vt.default_{name}=" + ",".join(f"0x{v}" for v in vals)
                    for name, vals in zip(("red", "grn", "blu"), chans))


def vt_file(rel, text, night):
    if not VT_PARAMS.search(text):
        return text, [f"{rel}: no vt.default_red/grn/blu parameters to set"]
    return VT_PARAMS.sub(vt_params(night), text), []


def noctalia_palette(pal):
    """Noctalia's custom palette JSON (docs/user/theming/palette.mdx, v5)."""
    out = {}
    for mode, p in (("dark", pal["night"]), ("light", pal["day"])):
        ansi = lambda black, white: {
            "black": p[black], "red": p["error"], "green": p["success"],
            "yellow": p["warning"], "blue": p["accent"], "magenta": p["violet"],
            "cyan": p["glow"], "white": p[white]}
        out[mode] = {
            "mPrimary": p["accent"], "mOnPrimary": p["on_accent"],
            "mSecondary": p["glow"], "mOnSecondary": p["on_accent"],
            "mTertiary": p["violet"], "mOnTertiary": p["on_accent"],
            "mError": p["error"], "mOnError": p["on_accent"],
            "mSurface": p["base"], "mOnSurface": p["text"],
            "mSurfaceVariant": p["surface"], "mOnSurfaceVariant": p["subtext"],
            "mOutline": p["border"], "mShadow": p["crust"],
            "mHover": p["overlay"], "mOnHover": p["text"],
            "terminal": {
                "background": p["base"], "foreground": p["text"],
                "cursor": p["text"], "cursorText": p["base"],
                "selectionBg": p["overlay"], "selectionFg": p["text"],
                "normal": ansi("crust", "subtext"),
                "bright": ansi("border", "text"),
            },
        }
    return json.dumps(out, indent=2) + "\n"


def contrast_errors(pal):
    def lum(h):
        def c(v):
            v /= 255
            return v / 12.92 if v <= 0.04045 else ((v + 0.055) / 1.055) ** 2.4
        r, g, b = (int(h[i:i + 2], 16) for i in (1, 3, 5))
        return 0.2126 * c(r) + 0.7152 * c(g) + 0.0722 * c(b)

    errors = []
    for mode, p in pal.items():
        for fgs, bgs, floor in CONTRAST:
            for fg in fgs:
                for bg in bgs:
                    hi, lo = sorted((lum(p[fg]), lum(p[bg])), reverse=True)
                    ratio = (hi + 0.05) / (lo + 0.05)
                    if ratio < floor:
                        errors.append(f"{PALETTE.name} [{mode}]: {fg} on {bg} is {ratio:.2f}:1, below {floor}:1")
    return errors


def main():
    check = sys.argv[1:] == ["--check"]
    if sys.argv[1:] not in ([], ["--check"]):
        sys.exit("usage: gen-theme.py [--check]")
    pal = load_palette()
    errors = contrast_errors(pal)
    outputs = {}
    for rel in RULES:
        text = (ROOT / rel).read_text()
        new, errs = theme_file(rel, text, pal["night"])
        errors += errs
        outputs[rel] = (text, new)
    for rel in VT_PALETTE:
        text = (ROOT / rel).read_text()
        new, errs = vt_file(rel, text, pal["night"])
        errors += errs
        outputs[rel] = (text, new)
    path = ROOT / NOCTALIA
    outputs[NOCTALIA] = (path.read_text() if path.exists() else None, noctalia_palette(pal))
    if errors:
        sys.exit("\n".join(errors))
    drift = [rel for rel, (old, new) in outputs.items() if old != new]
    if check:
        if drift:
            sys.exit("out of date with theme/palette.toml (run python3 tools/gen-theme.py):\n  "
                     + "\n  ".join(drift))
        print(f"theme: {len(outputs)} files match theme/palette.toml")
        return
    for rel in drift:
        (ROOT / rel).parent.mkdir(parents=True, exist_ok=True)
        (ROOT / rel).write_text(outputs[rel][1])
        print(f"wrote {rel}")


if __name__ == "__main__":
    main()
