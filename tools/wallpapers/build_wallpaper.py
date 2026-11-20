#!/usr/bin/env python3
"""Isotope build wallpaper ... one picture per build, drawn from its sha.

A night lake. On the far shore stand forty lights, one for each hex digit of
the build's 40 character fingerprint, and above each light a pillar of light
rises. The height of pillar k is hex digit k: 0 is the shortest, f the
tallest. The whole fingerprint is engraved small in the lower right corner.

    python3 build_wallpaper.py --seed <sha> [--mode day] [--preview]

Same seed in, same file out. Needs numpy, scipy and Pillow (requirements.txt) and lib.py beside it.
"""
import sys, os, re, hashlib, argparse
import numpy as np
from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from lib import *   # noqa: E402  (shared renderer of the element wallpapers)

HERE = os.path.dirname(os.path.abspath(__file__))   # lib exports its own HERE

FONT_MONO = os.path.join(HERE, 'fonts', 'JetBrainsMono-Regular.ttf')
FONT_SANS = os.path.join(HERE, 'fonts', 'Inter-Medium.otf')
NPILLAR = 40


def fingerprint(seed):
    """40 hex digits. A full git sha1 is used as it is, anything else is hashed."""
    s = seed.strip().lower()
    if re.fullmatch(r'[0-9a-f]{40}', s):
        return s
    return hashlib.sha1(seed.encode('utf-8')).hexdigest()


def scene_params(fp):
    """Everything that is not a pillar height, drawn from a hash of the fingerprint."""
    d = hashlib.sha256(('isotope-build-wallpaper/1:' + fp).encode()).digest()
    rng = np.random.default_rng(int.from_bytes(d[:8], 'big'))
    p = dict(
        yh=0.565 + 0.055 * rng.random(),          # horizon
        hue=rng.random(),                         # sky from teal blue to violet
        field=0.735 + 0.035 * rng.random(),        # where the row of lights starts (in screen heights)
        mtn_amp=0.09 + 0.13 * rng.random(),       # backdrop range
        mtn_peak=0.25 + 1.35 * rng.random(),      # where it is highest
        mist=0.35 + 0.65 * rng.random(),
        breeze=0.6 + 0.9 * rng.random(),          # how much the water breaks the reflections
        star_seed=int(rng.integers(1, 1 << 30)),
        s1=int(rng.integers(1, 9000)), s2=int(rng.integers(1, 9000)), s3=int(rng.integers(1, 9000)),
        bright=0.38 + 0.62 * rng.random(NPILLAR) ** 1.4,
        tint=rng.random(NPILLAR),
        width=0.85 + 0.4 * rng.random(NPILLAR),
        basej=rng.random(NPILLAR),
    )
    return p


def text_mask(cv, lines):
    """Render lines of (text, font path, px size at 2160p, tracking em, alpha), right aligned."""
    S = cv.S
    m = Image.new('L', (cv.W, cv.H), 0)
    d = ImageDraw.Draw(m)
    xr = cv.W - round(0.030 * cv.W)
    y = round(0.925 * cv.H)
    xl = xr
    for text, path, size, track, alpha in lines:
        font = ImageFont.truetype(path, max(8, round(size * S)))
        adv = [font.getlength(ch) + track * size * S for ch in text]
        x = xr - sum(adv) + track * size * S
        xl = min(xl, x)
        for ch, a in zip(text, adv):
            d.text((round(x), y), ch, font=font, fill=round(255 * alpha))
            x += a
        y += round(size * S * 1.75)
    return np.asarray(m, F32) / 255.0, xl


def mark_mask(cv, cx, cy, size):
    """The Isotope mark (M1) as a small engraving.

    mark-m1-256.png is the alpha of docs/assets/isotope-mark.svg, rasterised once
    with cairosvg, shell at 70% and the pushed-out side and core at 100%.
    """
    n = max(8, int(round(size)))
    m = Image.open(os.path.join(HERE, 'mark-m1-256.png')).convert('L').resize((n, n), Image.LANCZOS)
    out = np.zeros((cv.H, cv.W), F32)
    x0 = int(round(cx - n / 2)); y0 = int(round(cy - n / 2))
    out[y0:y0 + n, x0:x0 + n] = np.asarray(m, F32) / 255.0
    return out


def render(cv, seed, mode='night', stamp=True):
    day = mode == 'day'
    fp = fingerprint(seed)
    nib = np.array([int(c, 16) for c in fp])
    p = scene_params(fp)
    W, H, S = cv.W, cv.H, cv.S
    u, v = cv.u, cv.v
    U = cv.u[0]
    yh = p['yh']

    # ---------------- where the forty lights stand: ten groups of four
    pitch, gap = 0.0200, 0.0165
    k = np.arange(NPILLAR)
    pu = p['field'] + k * pitch + (k // 4) * gap
    span0, span1 = pu[0], pu[-1]
    mid = 0.5 * (span0 + span1)

    # ---------------- sky
    t = sstep(0.0, yh, v)
    if day:
        sky = mix(hexc('CDCEC7'), hexc('EEEFEB'), t ** 1.3)
        sky += (np.exp(-np.abs(v - yh) / 0.10) * np.exp(-((u - mid) / 0.8) ** 2) * 0.10)[..., None] * hexc('F9F1AF')
    else:
        hor = mix(hexc('2E2D27'), hexc('38342A'), p['hue'])       # neutral graphite to a warmer, sandier one
        sky = mix(PAL['crust'] * 0.85, hor, t ** 1.9)
        glowc = mix(hexc('665F48'), hexc('70654A'), p['hue'])
        town = np.exp(-((u - mid) / 0.62) ** 2)
        sky += (np.exp(-np.abs(v - yh) / 0.15) * (0.35 + 0.65 * town) * 0.085)[..., None] * glowc
        sky += (np.exp(-np.abs(v - yh) / 0.035) * town * 0.045)[..., None] * hexc('93854E')
        st = stars(cv, density=0.00024, seed=p['star_seed'])
        st *= sstep(yh - 0.03, yh - 0.30, v) * (0.30 + 0.70 * sstep(0.2, 1.2, u))
        sky += (st * 0.6)[..., None] * hexc('F0EEE2')
    air = blur(sky, 80 * S, 40 * S)

    # ---------------- far shore: a range, low hills, a line of spruce
    near = hexc('5E5C50') if day else PAL['crust'] * 0.45
    env = np.exp(-((U - p['mtn_peak']) / 0.55) ** 2)
    r0 = ridge(cv, p['s1'], 0.012, p['mtn_amp'] * 1.6, 1.3, yh, mid, 0.5, 0.0)
    r0 = yh - (yh - r0) * (0.30 + 0.70 * env)
    r1 = ridge(cv, p['s2'], 0.014, 0.060, 2.6, yh, mid, 0.4, 0.5, trees=0.0045)
    r2 = ridge(cv, p['s3'], 0.008, 0.020, 4.0, yh, mid, 0.3, 0.3, trees=0.013, tree_sp=0.0040)
    for rv, haze in ((r0, 0.70), (r1, 0.40), (r2, 0.06)):
        m = sstep(-0.7 / H, 0.7 / H, v - rv[None, :])
        col = mix(near, air, haze)
        fog = np.exp(-(yh - v) / 0.012) * p['mist']
        col = mix(col, air * 1.08, np.clip(fog, 0, 0.85))
        sky = mix(sky, col, m)

    # ---------------- the lights and their pillars
    limew = PAL['brand_glow']
    hmin, hstep = 0.030, 0.0190
    pil = np.zeros((H, W, 3), F32)
    src = np.zeros((H, W, 3), F32)
    R = int(70 * S)
    for i in range(NPILLAR):
        cx = pu[i] * H
        base_v = yh - 0.0035 - 0.004 * p['basej'][i]
        hgt = hmin + hstep * nib[i]
        tn = p['tint'][i]
        c = mix(PAL['brand'], PAL['brand_glow'], min(1.0, tn * 1.3)) if tn < 0.80 else hexc('F3F0DE')
        if day:
            c = mix(PAL['brand_day'], PAL['brand_deep'], tn)
        xa, xb = max(0, int(cx) - R), min(W, int(cx) + R + 1)
        if xa >= xb:
            continue
        ya, yb = max(0, int((base_v - hgt - 0.02) * H)), min(H, int((base_v + 0.012) * H))
        xs = cv.px[ya:yb, xa:xb] - cx
        tt = (base_v - cv.v[ya:yb, xa:xb]) / hgt            # 0 at the light, 1 at the top
        sig = 3.1 * S * p['width'][i] * (1 + 0.9 * np.clip(tt, 0, 1))
        cross = np.exp(-0.5 * (xs / sig) ** 2) + 0.20 * np.exp(-0.5 * (xs / (sig * 3.6)) ** 2)
        breath = 0.80 + 0.40 * vnoise(np.full_like(tt, i * 3.7), tt * 3.0 + i, seed=7)
        along = (0.22 + 0.78 * np.exp(-np.clip(tt, 0, 1) * 2.2)) * sstep(1.0, 0.62, tt) * sstep(-0.02, 0.03, tt)
        a = cross * along * breath * p['bright'][i]
        pil[ya:yb, xa:xb] += a[..., None] * c
        # the lamp itself
        dd = (xs / (3.2 * S)) ** 2 + ((cv.py[ya:yb, xa:xb] - base_v * H) / (2.6 * S)) ** 2
        src[ya:yb, xa:xb] += (np.exp(-dd) * p['bright'][i])[..., None] * mix(c, hexc('FFFFFF'), 0.5)
    # banks of haze drift through the row, so no two pillars are lit alike
    hz = fbm(u * 2.4 + 3.1, v * 5.0, 4, seed=p['s3'] + 9)
    veil = 0.50 + 0.80 * sstep(0.25, 0.75, hz)
    pil *= veil[..., None]
    pil = pil * 0.8 + blur(pil, 14 * S, 5 * S) * 0.5          # light spreading sideways in the haze
    if day:
        # by day the lamps are off and the pillars read as fine green threads in the haze
        amt = np.clip(pil.max(-1) / PAL['brand_deep'].max() * 0.75, 0, 0.62)
        sky = mix(sky, PAL['brand_day'] * 0.9, amt)
    else:
        # the lamps light the mist that lies along the shore
        lampglow = blur(src, 46 * S, 9 * S)
        sky = sky + pil * 0.42 + src * 1.6 + lampglow * (5.0 * p['mist']) * PAL['brand'] / PAL['brand'].max()

    # low mist on the far water line
    mistn = fbm(u * 2.2, v * 40, 4, seed=p['s1'] + 3)
    mist = np.exp(-np.abs(v - (yh - 0.003)) / 0.009) * (0.25 + 0.8 * mistn) * p['mist']
    sky = mix(sky, blur(sky, 60 * S, 14 * S) * 1.05 + air * 0.3, np.clip(mist, 0, 0.7) * sstep(yh + 0.004, yh - 0.02, v))

    # ---------------- water
    f = 1.15 * H
    yhp = yh * H
    dyp = np.maximum(cv.py - yhp, 0.35)
    tana = dyp / f
    sina = tana / np.sqrt(1 + tana * tana)
    z = 1.0 / tana
    xw = (cv.px - W / 2) / f * z
    foot = z * z / f
    disp_y = np.zeros((H, W), F32); disp_x = np.zeros((H, W), F32); resolved = np.zeros((H, W), F32)
    for i, (lam, amp) in enumerate([(2.6, 0.006), (0.9, 0.006), (0.33, 0.005), (0.12, 0.004)]):
        fade = sstep(1.5, 5.0, lam / foot)
        n = fbm(xw * 0.33 / lam + 11 * i, z / lam + 5 * i, 2, seed=p['s2'] + i) - 0.5
        disp_y += 2 * f * amp * p['breeze'] * 2 * n * fade
        n2 = vnoise(xw * 0.5 / lam + 3 * i, z / lam + 9 * i, seed=p['s3'] + i) - 0.5
        disp_x += 2 * f * amp * p['breeze'] * 2 * n2 * fade
        resolved += fade / 4
    my = np.minimum(2 * yhp - cv.py + disp_y, yhp - 0.5)
    mx = cv.px + disp_x * sina
    sky_soft = blur(sky, 2.0 * S, 12 * S)
    sky_far = blur(sky, 3.5 * S, 40 * S * p['breeze'])
    refl = mix(sample(sky_far, mx, my), sample(sky_soft, mx, my), resolved[..., None] ** 0.7)
    fres = 0.04 + 0.96 * (1 - sina) ** 4.5
    deep = hexc('6B6E62') * 0.60 if day else hexc('0D0E0B') * 0.60
    water = refl * fres[..., None] * (0.97 if day else 0.92) + deep * (1 - fres[..., None])
    wm = np.exp(-(cv.py - yhp) / (0.018 * H)) * (0.30 + 0.5 * fbm(u * 2.2, v * 40, 3, seed=p['s1'] + 4)) * p['mist']
    water = mix(water, sample(air, cv.px, np.full_like(cv.py, yhp - 6 * S)), np.clip(wm, 0, 0.7))
    img = mix(sky, water, sstep(-0.6, 0.6, cv.py - yhp))

    # ---------------- grade, then the stamp, engraved under the grain
    if day:
        img = bloom(img, S, thresh=0.75, gains=(0.10, 0.08, 0.06, 0.04))
    else:
        img = bloom(img, S, thresh=0.20, radii=(6, 22, 70, 220), gains=(0.30, 0.22, 0.14, 0.09))
    if stamp:
        label = 'ISOTOPE BUILD'
        s = seed.strip()
        if s.lower() != fp and re.fullmatch(r'[\x21-\x7e]{1,24}', s):
            label += '  ' + s
        sha = ' '.join(fp[i:i + 4] for i in range(0, 40, 4))
        tm, xl = text_mask(cv, [(label, FONT_SANS, 21, 0.16, 0.55), (sha, FONT_MONO, 24, 0.0, 0.80)])
        tm = np.maximum(tm, 0.70 * mark_mask(cv, xl - 46 * S, 0.925 * H + 37 * S, 62 * S))
        ink = hexc('1C1D19') if day else PAL['dim']
        img = mix(img, ink * (1.0 if day else 0.55), tm * (0.75 if day else 0.9))
    if day:
        return finish(img, cv, seed=int(fp[:6], 16), vignette=0.16, grain=0.008)
    return finish(img, cv, seed=int(fp[:6], 16), vignette=0.40, grain=0.011, lift=PAL['crust'] * 0.30)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--seed', required=True, help='git sha (40 hex digits are used as they are) or any string')
    ap.add_argument('--mode', default='night', choices=['night', 'day'])
    ap.add_argument('--preview', action='store_true', help='fast 1280x720 draft into preview/')
    ap.add_argument('--size', default='both', choices=['both', '1080p'], help='both: 4K plus its 1080p downsample')
    ap.add_argument('--no-stamp', action='store_true', help='leave the engraved sha out')
    ap.add_argument('--out', default=HERE, help='output directory')
    a = ap.parse_args()
    fp = fingerprint(a.seed)
    name = f'isotope-build-{fp[:12]}-{a.mode}'
    os.makedirs(a.out, exist_ok=True)
    if a.preview:
        os.makedirs(os.path.join(a.out, 'preview'), exist_ok=True)
        cv = Canvas(1280, 720)
        path = os.path.join(a.out, 'preview', name + '.png')
        Image.fromarray(to8(render(cv, a.seed, a.mode, not a.no_stamp))).save(path)
        print('wrote', path)
    elif a.size == '1080p':
        cv = Canvas(1920, 1080)
        save(render(cv, a.seed, a.mode, not a.no_stamp), os.path.join(a.out, name + '-1920x1080.png'), also_half=False)
    else:
        cv = Canvas(3840, 2160)
        save(render(cv, a.seed, a.mode, not a.no_stamp), os.path.join(a.out, name + '-3840x2160.png'))
    print('fingerprint', fp)


if __name__ == '__main__':
    main()
