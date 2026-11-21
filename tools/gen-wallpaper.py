#!/usr/bin/env python3
"""Draw the Isotope wallpapers from theme/palette.toml.

"Tiles": the Tessellate pattern of the Kairos design system
(github.com/kairos-io/design). A hex grid whose tiles are split into three
rhombi, the faces of a cube. Near the right of the frame some faces are filled
in the three brand tones, and the fill thins out to a faint outline grid on the
left, which stays calm for the first tiled window and the desktop icons. The
Isotope mark (M1: a hexagon shell, its sixth side lifted off, a core) sits in the field as
its focal point. Under it all is the GRUB background's recipe: a vertical
base -> crust gradient with a faint brand glow above the frame, a light
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

    gen-wallpaper.py --blur SRC.png OUT.png

writes a soft copy of a wallpaper for the COSMIC login screen (the greeter
patch in patches/cosmic/cosmic-greeter shows it behind the clock and the
password field): a Gaussian blur of radius (sigma) BLUR_SIGMA pixels at
3840x2160, scaled with the picture, at a quarter of SRC's size. It is made
cheaply, still with the stdlib only: SRC is averaged down by BLUR_STEP, blurred
there with three box passes (a close match to a Gaussian), scaled up to the
output size bilinearly and dithered. SRC is an 8-bit RGB or RGBA PNG without
interlacing, which is what the committed wallpapers and the generators write.
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


def read_png(path):
    """Width, height and rows of RGB bytes of an 8-bit, non-interlaced RGB or
    RGBA PNG (alpha is dropped)."""
    data = Path(path).read_bytes()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path}: not a PNG")
    pos, idat, hdr = 8, [], None
    while pos < len(data):
        n, = struct.unpack(">I", data[pos:pos + 4])
        t, c = data[pos + 4:pos + 8], data[pos + 8:pos + 8 + n]
        pos += 12 + n
        if t == b"IHDR":
            hdr = struct.unpack(">IIBBBBB", c)
        elif t == b"IDAT":
            idat.append(c)
        elif t == b"IEND":
            break
    if hdr is None:
        raise ValueError(f"{path}: no IHDR")
    w, h, depth, ctype, _, _, interlace = hdr
    if depth != 8 or ctype not in (2, 6) or interlace:
        raise ValueError(f"{path}: only 8-bit RGB/RGBA PNGs without interlacing are read")
    bpp = 3 if ctype == 2 else 4
    raw = zlib.decompress(b"".join(idat))
    stride = w * bpp
    prev = bytearray(stride)
    rows = []
    for y in range(h):
        f = raw[y * (stride + 1)]
        cur = bytearray(raw[y * (stride + 1) + 1:(y + 1) * (stride + 1)])
        if f == 1:
            for i in range(bpp, stride):
                cur[i] = (cur[i] + cur[i - bpp]) & 0xFF
        elif f == 2:
            cur = bytearray((a + b) & 0xFF for a, b in zip(cur, prev))
        elif f == 3:
            for i in range(bpp):
                cur[i] = (cur[i] + (prev[i] >> 1)) & 0xFF
            for i in range(bpp, stride):
                cur[i] = (cur[i] + ((cur[i - bpp] + prev[i]) >> 1)) & 0xFF
        elif f == 4:
            for i in range(bpp):
                cur[i] = (cur[i] + prev[i]) & 0xFF
            for i in range(bpp, stride):
                a, b, c = cur[i - bpp], prev[i], prev[i - bpp]
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                cur[i] = (cur[i] + (a if pa <= pb and pa <= pc else (b if pb <= pc else c))) & 0xFF
        elif f != 0:
            raise ValueError(f"{path}: bad filter {f} on row {y}")
        rows.append(bytes(cur) if bpp == 3 else bytes(b for i, b in enumerate(cur) if i % 4 != 3))
        prev = cur
    return w, h, rows


# The login screen's blur: sigma in pixels at 3840x2160 (scaled with the
# picture's height), the averaging step before the blur, and the output scale.
BLUR_SIGMA = 40.0
BLUR_STEP = 8
BLUR_OUT = 4


def box_blur(plane, w, h, r):
    """One box pass of radius r over a w x h plane of floats, rows then
    columns, the edges clamped."""
    n = 2 * r + 1
    out = [0.0] * (w * h)
    for y in range(h):
        o = y * w
        acc = sum(plane[o + min(max(x, 0), w - 1)] for x in range(-r, r + 1))
        for x in range(w):
            out[o + x] = acc / n
            acc += plane[o + min(x + r + 1, w - 1)] - plane[o + max(x - r, 0)]
    res = [0.0] * (w * h)
    for x in range(w):
        acc = sum(out[min(max(y, 0), h - 1) * w + x] for y in range(-r, r + 1))
        for y in range(h):
            res[y * w + x] = acc / n
            acc += out[min(y + r + 1, h - 1) * w + x] - out[max(y - r, 0) * w + x]
    return res


def blur_rows(w, h, rows):
    """The login screen's soft copy of a picture: (width, height, rows)."""
    step = BLUR_STEP
    sw, sh = max(1, w // step), max(1, h // step)
    planes = [[0.0] * (sw * sh) for _ in range(3)]
    k = 1.0 / (step * step)
    for sy in range(sh):
        sums = [[0] * sw for _ in range(3)]
        for y in range(sy * step, sy * step + step):
            row = rows[min(y, h - 1)]
            for c in range(3):
                ch = row[c::3]
                s = sums[c]
                for sx in range(sw):
                    s[sx] += sum(ch[sx * step:sx * step + step])
        for c in range(3):
            o = sy * sw
            pl, s = planes[c], sums[c]
            for sx in range(sw):
                pl[o + sx] = s[sx] * k
    # Three box passes of radius r approximate a Gaussian of sigma
    # sqrt(r * (r + 1)) (the variance of one pass is ((2r+1)^2 - 1) / 12).
    sigma = BLUR_SIGMA * h / 2160.0 / step
    r = max(1, round((math.sqrt(4 * sigma * sigma + 1) - 1) / 2))
    for c in range(3):
        for _ in range(3):
            planes[c] = box_blur(planes[c], sw, sh, r)
    ow, oh = max(1, w // BLUR_OUT), max(1, h // BLUR_OUT)
    fx, fy = sw / ow, sh / oh
    xs = []
    for x in range(ow):
        u = min(max((x + 0.5) * fx - 0.5, 0.0), sw - 1.0)
        x0 = min(int(u), sw - 2) if sw > 1 else 0
        xs.append((x0, min(x0 + 1, sw - 1), u - x0))
    seed = 0x9E3779B9
    out = []
    for y in range(oh):
        v = min(max((y + 0.5) * fy - 0.5, 0.0), sh - 1.0)
        y0 = min(int(v), sh - 2) if sh > 1 else 0
        y1, ty = min(y0 + 1, sh - 1), v - y0
        row = bytearray(ow * 3)
        for c in range(3):
            pl = planes[c]
            a, b = y0 * sw, y1 * sw
            for x, (x0, x1, tx) in enumerate(xs):
                top = pl[a + x0] + (pl[a + x1] - pl[a + x0]) * tx
                bot = pl[b + x0] + (pl[b + x1] - pl[b + x0]) * tx
                seed = (seed * 1103515245 + 12345) & 0x7FFFFFFF
                n = ((seed >> 8) & 0xFF) / 255.0 - ((seed >> 16) & 0xFF) / 255.0
                row[x * 3 + c] = max(0, min(255, int(top + (bot - top) * ty + n * 0.75 + 0.5)))
        out.append(bytes(row))
    return ow, oh, out


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


# The Isotope mark, M1, in docs/assets/isotope-mark.svg's 512 box: the
# hexagon's centre and corner radius, the shell's outer and inner corners
# (from the top, clockwise; the side from the top to the upper right is the
# one lifted off), the lifted side and the core.
M1_C, M1_R = (245.76, 273.61), 204.80
M1_OUT = [(245.76, 68.81), (423.12, 171.21), (423.12, 376.01), (245.76, 478.41), (68.40, 376.01), (68.40, 171.21)]
M1_IN = [(245.76, 104.29), (392.40, 188.95), (392.40, 358.28), (245.76, 442.94), (99.12, 358.28), (99.12, 188.95)]
M1_LID = [(266.24, 33.34), (443.60, 135.74), (412.88, 153.48), (266.24, 68.81)]
M1_CORE = 34.82


def mark_shapes(cx, cy, R):
    """The mark centred on (cx, cy) with corner radius R, as convex shapes:
    (points, tone) for the five sides of the shell and the lifted side, and
    the core as ((x, y, r), tone). Tone 0 is the light tone, 1 the brand."""
    k = R / M1_R
    f = lambda p: (cx + (p[0] - M1_C[0]) * k, cy + (p[1] - M1_C[1]) * k)
    shapes = []
    for i in range(1, 6):
        j = (i + 1) % 6
        shapes.append(([f(M1_OUT[i]), f(M1_OUT[j]), f(M1_IN[j]), f(M1_IN[i])], 1))
    shapes.append(([f(p) for p in M1_LID], 0))
    shapes.append(((cx, cy, M1_CORE * k), 0))
    return shapes


def shape_coverage(x, y, shape):
    """Anti-aliased coverage of pixel (x, y) by a convex polygon, or by a
    circle given as (x, y, r): the signed distance to the edge, as a
    one-pixel ramp."""
    if len(shape) == 3 and not isinstance(shape[0], tuple):
        sx, sy, r = shape
        return max(0.0, min(1.0, r - math.hypot(x - sx, y - sy) + 0.5))
    pts = shape
    n = len(pts)
    area = sum(pts[i][0] * pts[(i + 1) % n][1] - pts[(i + 1) % n][0] * pts[i][1] for i in range(n))
    sign = 1.0 if area > 0 else -1.0
    d = 1e9
    for i in range(n):
        ax, ay = pts[i]
        bx, by = pts[(i + 1) % n]
        ex, ey = bx - ax, by - ay
        # Positive inside: the same side of every edge as the rest.
        d = min(d, sign * (ex * (y - ay) - ey * (x - ax)) / math.hypot(ex, ey))
    return max(0.0, min(1.0, d + 0.5))


def draw(P, mode, W, H, plain, night_palette=None):
    """Rows of RGB bytes. night_palette gives the brand tones of the tiles
    and the mark; it defaults to P."""
    night = mode == "night"
    top, bot = rgb(P["base"]), rgb(P["crust"])
    acc = rgb(P["brand"])
    N = night_palette or P
    tones = (rgb(N["brand_glow"]), rgb(N["brand"]), rgb(N["brand_deep"]))
    # One light: the tiles are graphite, the cube's three faces three greys,
    # and the only colour is the light of the mark's core, which they catch
    # the nearer they are to it. Yellow is light here, never paint.
    faces = (rgb(P["muted"]), rgb(P["border"]), rgb(P["overlay"])) if night else \
        (rgb(P["border"]), rgb(P["dim"]), rgb(P["overlay"]))
    light = rgb(N["brand"])
    # The mark's two tones, as in docs/assets: the light tone and the brand
    # at night; on day, ink for the shell and the deep tone for the light,
    # since the light tones vanish on a light ground.
    mark_tones = (tones[0], tones[1]) if night else (rgb(N["brand_deep"]), rgb(P["text"]))
    line = rgb(P["border"])
    s = H / 1080.0
    glow_k = 0.10 if night else 0.06
    fill_a = 0.55 if night else 0.40
    line_a = 0.55 if night else 0.65

    r_t = 56.0 * s                      # tile radius
    lw = max(1.0, 1.1 * s)              # outline width
    apo = r_t * S3 / 2                  # tile apothem
    spokes = [(math.cos(math.radians(a)), math.sin(math.radians(a))) for a in (-150, -30, 90)]

    def bias(u, v):
        return max(0.0, 0.92 - math.hypot((u - 0.86) * W / H, v - 0.52) * 1.05)
    mcx, mcy, mR = W * 0.72, H * 0.50, 150.0 * s
    mark = mark_shapes(mcx, mcy, mR)
    # Tiles stay clear of the mark: a hexagon a little larger than it.
    clear_R = mR * 1.55
    # How far the core's light reaches, and how strong it is on the ground.
    reach = mR * 3.2
    halo_k = 0.16 if night else 0.07

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
            if not plain:
                ld = math.hypot(x - mcx, y - mcy) / reach
                lit = math.exp(-ld * ld * 2.2)
                if lit > 0.002:
                    hk = halo_k * lit
                    r += (light[0] - r) * hk; g += (light[1] - g) * hk; b += (light[2] - b) * hk
            else:
                lit = 0.0
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
                        lt = math.exp(-((tx - mcx) ** 2 + (tyy - mcy) ** 2) / reach ** 2 * 2.2)
                        f = faces[face]
                        # The face toward the light (the top one) catches most of it.
                        cat = lt * (0.55, 0.30, 0.18)[face]
                        col = mix(f, light, cat)
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
                    for shape, tone in mark:
                        c = shape_coverage(x + 0.5, y + 0.5, shape)
                        if c > 0:
                            col = mark_tones[tone]
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
    if len(sys.argv) >= 2 and sys.argv[1] == "--blur":
        if len(sys.argv) != 4:
            sys.exit("usage: gen-wallpaper.py --blur SRC.png OUT.png")
        w, h, rows = blur_rows(*read_png(sys.argv[2]))
        out = Path(sys.argv[3])
        out.parent.mkdir(parents=True, exist_ok=True)
        write_png(out, w, h, rows)
        print(f"wrote {out} ({w}x{h}, {out.stat().st_size} bytes)")
        return
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
