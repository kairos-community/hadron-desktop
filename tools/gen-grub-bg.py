#!/usr/bin/env python3
"""Generate the pictures of the Isotope GRUB theme.

    python3 tools/gen-grub-bg.py OUTDIR [SPLASH]

writes into OUTDIR (rootfs/etc/kairos/branding/isotope-theme in the repo):

- background.tga: the "One light" scene asleep, a dark shell lit only on its
  inner edge by the core. The boot splash draws it (`isotope-splash --grub`;
  SPLASH defaults to splash/isotope-splash, built with `make -C splash`), so
  GRUB and the splash show the same light. 1920x1080; theme.txt scales it to
  the screen's height and anchors it left.
- wordmark.tga: the isotope wordmark with alpha, the splash's own 1080p mask
  from splash/masks.h. It is a separate image so theme.txt places it in
  percent, over the menu column, on any screen size.
- progress_bar_c.tga, progress_highlight_c.tga: the countdown line, overlay
  and brand. GRUB draws a progress bar at least 28 px high whatever theme.txt
  says (gui_progress_bar.c), so the pixmaps are 28 px, clear but for a 3 px
  line at the foot.
- select_*.tga: the nine slices of the selected entry's box, a bar of light at
  the left and its glow fading to the right, with alpha.

TGA, not PNG: GRUB's png.mod rejects valid PNGs with "unsupported format",
which makes gfxmenu discard the whole theme; tga.mod decodes cleanly. Colours
from theme/palette.toml (brand roles). Stdlib only. Regenerate after a palette
or splash change with:

    make -C splash && python3 tools/gen-grub-bg.py rootfs/etc/kairos/branding/isotope-theme
"""
import math
import os
import struct
import subprocess
import sys
import tempfile
import tomllib
from pathlib import Path

W, H = 1920, 1080
BOX_W, BOX_H, BAR = 640, 52, 3       # the selected entry: item_height in theme.txt
LINE, BAR_MIN = 3, 28                # the countdown line, GRUB's least bar height

ROOT = Path(__file__).resolve().parent.parent


def tga(path, w, h, pixels, alpha):
    """pixels: rows top to bottom of (r, g, b) or (r, g, b, a)."""
    bpp = 32 if alpha else 24
    with open(path, "wb") as f:
        f.write(bytes([0, 0, 2, 0, 0, 0, 0, 0]) + struct.pack("<HHHH", 0, 0, w, h)
                + bytes([bpp, 0x20 | (8 if alpha else 0)]))
        out = bytearray()
        for p in pixels:
            out += bytes([p[2], p[1], p[0]] + ([p[3]] if alpha else []))
        f.write(out)


def background(out, splash):
    with tempfile.TemporaryDirectory() as d:
        ppm = os.path.join(d, "bg.ppm")
        subprocess.run([splash, "--render=" + ppm, "--size=%dx%d" % (W, H), "--grub"], check=True)
        data = open(ppm, "rb").read()
    head = b"P6\n%d %d\n255\n" % (W, H)
    assert data.startswith(head), "unexpected PPM from the splash"
    raw = data[len(head):]
    with open(out, "wb") as f:
        f.write(bytes([0, 0, 2, 0, 0, 0, 0, 0]) + struct.pack("<HHHH", 0, 0, W, H) + bytes([24, 0x20]))
        bgr = bytearray(raw)
        bgr[0::3], bgr[2::3] = raw[2::3], raw[0::3]
        f.write(bgr)


def wordmark(out, night):
    """WORDMARK[1] of splash/masks.h, decoded: the text colour, the dot of the
    i in brand_glow (the core again, as on the splash)."""
    import re
    src = (ROOT / "splash" / "masks.h").read_text()
    w, h, base, tx, ty, n, name = re.search(
        r"WORDMARK\[4\] = \{\s*\{[^}]*\},\s*\{ *(\d+), *(\d+), *(\d+), *(\d+), *(\d+), *(\d+), *(\w+) *\}", src).groups()
    w, h, tx, ty = int(w), int(h), int(tx), int(ty)
    body = re.search(r"%s\[\d+\] = \{([^}]*)\}" % name, src).group(1)
    data = [int(v) for v in body.replace("\n", "").split(",") if v.strip()]
    rgb = lambda c: tuple(int(c[i:i + 2], 16) for i in (1, 3, 5))
    text, dot = rgb(night["text"]), rgb(night["brand_glow"])
    cov = []
    for b in data:
        if b < 0x40: cov += [0] * (b + 1)
        elif b >= 0xc0: cov += [255] * (b - 0xbf)
        else: cov.append(round((b - 0x40) * 255 / 127))
    assert len(cov) == w * h, "wordmark mask does not decode to its size"
    px = [(*(dot if i % w < tx and i // w < ty else text), a) for i, a in enumerate(cov)]
    tga(out, w, h, px, True)
    return w, h, int(base)


def select_box(night):
    """The box as rows of RGBA: the bar in brand_glow, the fade in brand."""
    rgb = lambda h: tuple(int(h[i:i + 2], 16) for i in (1, 3, 5))
    bar, fade = rgb(night["brand_glow"]), rgb(night["brand"])
    row = []
    for x in range(BOX_W):
        a = 0.17 * math.exp(-x / 150) + 0.035 * max(0, 1 - x / BOX_W)
        row.append((*bar, 255) if x < BAR else (*fade, int(255 * a)))
    return [row] * BOX_H


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    outdir = Path(sys.argv[1])
    splash = sys.argv[2] if len(sys.argv) > 2 else str(ROOT / "splash" / "isotope-splash")
    with open(ROOT / "theme" / "palette.toml", "rb") as f:
        night = tomllib.load(f)["night"]
    background(outdir / "background.tga", splash)
    ww, wh, wb = wordmark(outdir / "wordmark.tga", night)
    box = select_box(night)
    # GRUB tiles the edges and stretches the centre: the west slice holds the
    # bar, the centre the fade. The one-pixel rows top and bottom are the same.
    cols = {"w": (0, BAR + 1), "c": (BAR + 1, BOX_W - 1), "e": (BOX_W - 1, BOX_W)}
    rows = {"n": (0, 1), "": (1, BOX_H - 1), "s": (BOX_H - 1, BOX_H)}
    for rn, (y0, y1) in rows.items():
        for cn, (x0, x1) in cols.items():
            name = (rn + cn) if rn else cn
            name = {"nc": "n", "sc": "s"}.get(name, name)
            part = [r[x0:x1] for r in box[y0:y1]]
            tga(outdir / ("select_%s.tga" % name), x1 - x0, y1 - y0, [p for r in part for p in r], True)
    rgb = lambda h: tuple(int(h[i:i + 2], 16) for i in (1, 3, 5))
    for name, role in (("progress_bar_c", "overlay"), ("progress_highlight_c", "brand")):
        px = [(0, 0, 0, 0)] * (BAR_MIN - LINE) + [(*rgb(night[role]), 255)] * LINE
        tga(outdir / (name + ".tga"), 1, BAR_MIN, px, True)
    print("wrote %s: background.tga (%dx%d), wordmark.tga (%dx%d, baseline %d) and select_*.tga"
          % (outdir, W, H, ww, wh, wb))


if __name__ == "__main__":
    main()
