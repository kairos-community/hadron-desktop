#!/usr/bin/env python3
"""Draw the Isotope wallpapers from theme/palette.toml.

"Tiles": the Tessellate pattern of the Kairos design system
(github.com/kairos-io/design). A hex grid whose tiles are split into three
rhombi, the faces of a cube. Near the right of the frame some faces are filled
in the three lime tones, and the fill thins out to a faint outline grid on the
left, which stays calm for the first tiled window and the desktop icons. The
Isotope mark (a hexagon of six triangles, one pushed out) sits in the field as
its focal point. Under it all is the GRUB background's recipe: a vertical
base -> crust gradient with a faint accent glow above the frame, a light
vignette, and triangular dithering, so the dark gradient does not band on
8-bit panels. The "plain" variant is the same background without the tiles and
the mark, for the lock and login screens.

The image is a pure function of the palette, the variant and the size: the
tile choice and the dither noise come from fixed seeds, and the PNG has no
timestamp, so two runs write the same bytes. The Dockerfile runs this at build
time (stdlib only, no network), so an edit to theme/palette.toml reaches the
wallpapers too.

    gen-wallpaper.py OUTDIR NAME...

NAME is isotope-{tiles|plain}-{night|day}-WIDTHxHEIGHT; each is written to
OUTDIR/NAME.png. For example:

    python3 tools/gen-wallpaper.py /tmp/wp isotope-tiles-night-1920x1080
"""
import math
import re
import struct
import sys
import tomllib
import zlib
from pathlib import Path

PALETTE = Path(__file__).resolve().parent.parent / "theme" / "palette.toml"
NAME = re.compile(r"isotope-(tiles|plain)-(night|day)-([1-9][0-9]{2,4})x([1-9][0-9]{2,4})")
S3 = math.sqrt(3.0)
# The tiles and the mark are brand, not theme: they take the night palette's
# brand roles in both modes, as a brand mark does, and never the theme accent.


def rgb(h):
    return tuple(int(h[i:i + 2], 16) for i in (1, 3, 5))


def mix(a, b, t):
    return tuple(a[i] + (b[i] - a[i]) * t for i in range(3))


def write_png(path, w, h, rows):
    def chunk(t, d):
        c = struct.pack(">I", len(d)) + t + d
        return c + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    raw = bytearray()
    for r in rows:
        raw += b"\x00" + r
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n")
        f.write(chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)))
        f.write(chunk(b"IDAT", zlib.compress(bytes(raw), 6)))
        f.write(chunk(b"IEND", b""))


def tile_hash(q, r, face):
    """A fixed pseudo-random number in [0, 1) for one face of one tile."""
    h = (q * 73856093) ^ (r * 19349663) ^ (face * 83492791) ^ 0x5BD1E995
    h &= 0xFFFFFFFF
    h ^= h >> 13
    h = (h * 0x5BD1E995) & 0xFFFFFFFF
    h ^= h >> 15
    return (h & 0xFFFFFF) / float(0x1000000)


def hex_round(fq, fr):
    """Cube rounding of fractional axial coordinates to the nearest tile."""
    fs = -fq - fr
    q, r, s = round(fq), round(fr), round(fs)
    dq, dr, ds = abs(q - fq), abs(r - fr), abs(s - fs)
    if dq > dr and dq > ds:
        q = -r - s
    elif dr > ds:
        r = -q - s
    return q, r


def mark_triangles(cx, cy, R):
    """The Isotope mark: six triangles of a pointy-top hexagon around
    (cx, cy), each shrunk toward its own centre so a gap shows between them,
    the first pushed out along its outward normal. Returns (points, tone)
    with tone 0, 1 or 2 (light, mid, deep)."""
    v = [(cx + R * math.cos(math.radians(-90 + 60 * i)),
          cy + R * math.sin(math.radians(-90 + 60 * i))) for i in range(6)]
    tris = []
    for i in range(6):
        a, b = v[i], v[(i + 1) % 6]
        pts = [(cx, cy), a, b]
        if i == 0:
            mx, my = (a[0] + b[0]) / 2 - cx, (a[1] + b[1]) / 2 - cy
            m = math.hypot(mx, my)
            d = 0.2 * R
            pts = [(p[0] + mx / m * d, p[1] + my / m * d) for p in pts]
        gx = sum(p[0] for p in pts) / 3
        gy = sum(p[1] for p in pts) / 3
        k = 0.94
        pts = [(gx + (p[0] - gx) * k, gy + (p[1] - gy) * k) for p in pts]
        tris.append((pts, i % 3))
    return tris


def tri_coverage(x, y, pts):
    """Anti-aliased coverage of pixel (x, y) by a triangle: the signed
    distance to its nearest edge, as a one-pixel ramp."""
    d = 1e9
    area = ((pts[1][0] - pts[0][0]) * (pts[2][1] - pts[0][1])
            - (pts[1][1] - pts[0][1]) * (pts[2][0] - pts[0][0]))
    sign = 1.0 if area > 0 else -1.0
    for i in range(3):
        ax, ay = pts[i]
        bx, by = pts[(i + 1) % 3]
        ex, ey = bx - ax, by - ay
        # Positive inside: the same side of every edge as the third vertex.
        dd = sign * (ex * (y - ay) - ey * (x - ax)) / math.hypot(ex, ey)
        d = min(d, dd)
    return max(0.0, min(1.0, d + 0.5))


def draw(P, mode, W, H, plain, night_palette=None):
    """Rows of RGB bytes. night_palette gives the brand tones of the tiles
    and the mark; it defaults to P."""
    night = mode == "night"
    top, bot = rgb(P["base"]), rgb(P["crust"])
    acc = rgb(P["brand"])
    N = night_palette or P
    tones = (rgb(N["brand_glow"]), rgb(N["brand"]), rgb(N["brand_deep"]))
    line = rgb(P["border"])
    s = H / 1080.0
    glow_k = 0.10 if night else 0.06
    fill_a = 0.30 if night else 0.34
    line_a = 0.55 if night else 0.65

    r_t = 56.0 * s                      # tile radius
    lw = max(1.0, 1.1 * s)              # outline width
    apo = r_t * S3 / 2                  # tile apothem
    spokes = [(math.cos(math.radians(a)), math.sin(math.radians(a))) for a in (-150, -30, 90)]

    def bias(u, v):
        return max(0.0, 0.92 - math.hypot((u - 0.86) * W / H, v - 0.52) * 1.05)
    mcx, mcy, mR = W * 0.72, H * 0.50, 150.0 * s
    mark = mark_triangles(mcx, mcy, mR)
    # Tiles stay clear of the mark: a hexagon a little larger than it.
    clear_R = mR * 1.55

    gcx, gcy, grad = W * 0.5, -H * 0.15, H * 0.95
    seed = 0x9E3779B9
    rows = []
    for y in range(H):
        ty = y / (H - 1)
        base = mix(top, bot, ty)
        row = bytearray(W * 3)
        dy0 = y - gcy
        near_mark = mark and abs(y - mcy) < mR * 1.3
        for x in range(W):
            r, g, b = base
            dx = x - gcx
            k = 1.0 - math.sqrt(dx * dx + dy0 * dy0) / grad
            if k > 0:
                k = k * k * glow_k
                r += (acc[0] - r) * k; g += (acc[1] - g) * k; b += (acc[2] - b) * k
            if not plain:
                # The tile under (x, y), in axial coordinates.
                fq = (S3 / 3 * x - y / 3) / r_t
                fr = (2 / 3 * y) / r_t
                q, rr = hex_round(fq, fr)
                tx = r_t * S3 * (q + rr / 2)
                tyy = r_t * 1.5 * rr
                ex, ey = x - tx, y - tyy
                mdx, mdy = tx - mcx, tyy - mcy
                clear = mark and max(abs(mdx), abs(mdx * 0.5 + mdy * S3 / 2),
                                     abs(-mdx * 0.5 + mdy * S3 / 2)) < clear_R * S3 / 2
                if not clear:
                    # One value per tile, so a face is filled whole or not.
                    t = bias(tx / W, tyy / (H - 1))
                    a = math.degrees(math.atan2(ey, ex))
                    face = 0 if -150 <= a < -30 else (2 if -30 <= a < 90 else 1)
                    if tile_hash(q, rr, face) < t:
                        fa = fill_a * min(1.0, 0.35 + t)
                        col = tones[face]
                        r += (col[0] - r) * fa; g += (col[1] - g) * fa; b += (col[2] - b) * fa
                    # Outline: the hexagon's edge and the three spokes.
                    e = apo - max(abs(ex), abs(ex * 0.5 + ey * S3 / 2), abs(-ex * 0.5 + ey * S3 / 2))
                    for sx, sy in spokes:
                        if ex * sx + ey * sy > 0:
                            e = min(e, abs(ex * sy - ey * sx))
                    if e < lw + 1:
                        la = line_a * (0.25 + 0.75 * min(1.0, bias(x / W, ty) * 1.6)) * max(0.0, min(1.0, lw + 0.5 - e))
                        r += (line[0] - r) * la; g += (line[1] - g) * la; b += (line[2] - b) * la
                if near_mark and abs(x - mcx) < mR * 1.4:
                    for pts, tone in mark:
                        c = tri_coverage(x, y, pts)
                        if c > 0:
                            col = tones[tone]
                            r += (col[0] - r) * c; g += (col[1] - g) * c; b += (col[2] - b) * c
            # vignette
            vx = (x / W - 0.5) * 2; vy = (ty - 0.5) * 2
            v2 = (vx * vx + vy * vy) * (0.10 if night else 0.02)
            r *= 1 - v2; g *= 1 - v2; b *= 1 - v2
            # dither: +/- 0.75 LSB triangular noise from a fixed-seed LCG
            seed = (seed * 1103515245 + 12345) & 0x7FFFFFFF
            n = ((seed >> 8) & 0xFF) / 255.0 - ((seed >> 16) & 0xFF) / 255.0
            i = x * 3
            row[i] = max(0, min(255, int(r + n * 0.75 + 0.5)))
            row[i + 1] = max(0, min(255, int(g + n * 0.75 + 0.5)))
            row[i + 2] = max(0, min(255, int(b + n * 0.75 + 0.5)))
        rows.append(bytes(row))
    return rows


def main():
    if len(sys.argv) < 3:
        sys.exit("usage: gen-wallpaper.py OUTDIR isotope-{tiles|plain}-{night|day}-WIDTHxHEIGHT...")
    out = Path(sys.argv[1])
    with open(PALETTE, "rb") as f:
        pal = tomllib.load(f)
    jobs = []
    for name in sys.argv[2:]:
        m = NAME.fullmatch(name)
        if not m:
            sys.exit(f"gen-wallpaper: {name!r} is not isotope-{{tiles|plain}}-{{night|day}}-WIDTHxHEIGHT")
        jobs.append((name, m.group(1) == "plain", m.group(2), int(m.group(3)), int(m.group(4))))
    out.mkdir(parents=True, exist_ok=True)
    for name, plain, mode, w, h in jobs:
        path = out / f"{name}.png"
        write_png(path, w, h, draw(pal[mode], mode, w, h, plain, night_palette=pal["night"]))
        print(f"wrote {path} ({path.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
