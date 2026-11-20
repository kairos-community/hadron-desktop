"""Shared rendering helpers for the Isotope element wallpapers.

Everything is drawn with numpy shaders in linear light, then graded,
grained, dithered and written as 8-bit sRGB PNG. No external images.
"""
import sys, os, argparse
import numpy as np
from scipy import ndimage as ndi
from PIL import Image

F32 = np.float32
HERE = os.path.dirname(os.path.abspath(__file__))


# ---------------------------------------------------------------- colour
def srgb2lin(c):
    c = np.asarray(c, F32)
    return np.where(c <= 0.04045, c / 12.92, ((c + 0.055) / 1.055) ** 2.4).astype(F32)


def lin2srgb(c):
    c = np.clip(c, 0, None)
    return np.where(c <= 0.0031308, c * 12.92, 1.055 * np.power(c, 1 / 2.4) - 0.055).astype(F32)


def hexc(s):
    s = s.lstrip('#')
    return srgb2lin([int(s[i:i + 2], 16) / 255 for i in (0, 2, 4)])


PAL = {k: hexc(v) for k, v in dict(
    # warm graphite night palette (replaced the earlier blue-violet one)
    crust='0C0D0B', base='141411', surface='1D1D1A', overlay='2A2B25',
    border='393A34', muted='595B52', dim='909287', subtext='CED0C8',
    text='F4F5F2',
    # Autunite: the brand yellow, used as light (replaced the lime)
    brand='E4DE48', brand_glow='F9F1AF', brand_deep='9F9615', brand_day='766F08', ivory='E9E7C1',
    lime_l='C6F36B', lime='8FD12E', lime_d='4E9A06',
    green='3B7504', violet='B49CF2',
    day_base='F4F5F2', day_surface='FFFFFF', day_crust='D8D9D3').items()}


def mix(a, b, t):
    t = np.asarray(t, F32)
    if t.ndim == 2 and (np.ndim(a) == 1 or np.ndim(b) == 1 or np.ndim(a) == 3 or np.ndim(b) == 3):
        t = t[..., None]
    return a * (1 - t) + b * t


def sstep(a, b, x):
    t = np.clip((x - a) / (b - a), 0, 1)
    return (t * t * (3 - 2 * t)).astype(F32)


# ---------------------------------------------------------------- noise
_TAB = {}


def _tab(seed):
    if seed not in _TAB:
        _TAB[seed] = np.random.default_rng(seed).random((256, 256), dtype=F32)
    return _TAB[seed]


def vnoise(x, y, seed=0):
    """Value noise in 0..1, quintic interpolation, period 256."""
    t = _tab(seed)
    xi = np.floor(x); yi = np.floor(y)
    fx = (x - xi).astype(F32); fy = (y - yi).astype(F32)
    xi = xi.astype(np.int64) & 255; yi = yi.astype(np.int64) & 255
    x1 = (xi + 1) & 255; y1 = (yi + 1) & 255
    ux = fx * fx * fx * (fx * (fx * 6 - 15) + 10)
    uy = fy * fy * fy * (fy * (fy * 6 - 15) + 10)
    a = t[yi, xi]; b = t[yi, x1]; c = t[y1, xi]; d = t[y1, x1]
    return (a + (b - a) * ux + (c - a) * uy + (a - b - c + d) * ux * uy).astype(F32)


def fbm(x, y, octaves=5, lac=2.03, gain=0.5, seed=0):
    """Fractal noise, roughly 0..1. Octaves are rotated to hide the lattice."""
    out = np.zeros(np.broadcast(x, y).shape, F32); amp = 1.0; tot = 0.0
    c, s = np.cos(0.6), np.sin(0.6)
    for i in range(octaves):
        out += amp * vnoise(x, y, seed + i)
        tot += amp; amp *= gain
        x, y = (c * x - s * y) * lac + 17.3, (s * x + c * y) * lac + 5.1
    return out / tot


def ridged(x, y, octaves=5, lac=2.03, gain=0.5, seed=0):
    out = np.zeros(np.broadcast(x, y).shape, F32); amp = 1.0; tot = 0.0
    c, s = np.cos(0.6), np.sin(0.6)
    for i in range(octaves):
        n = 1 - np.abs(2 * vnoise(x, y, seed + i) - 1)
        out += amp * n * n
        tot += amp; amp *= gain
        x, y = (c * x - s * y) * lac + 17.3, (s * x + c * y) * lac + 5.1
    return out / tot


# ---------------------------------------------------------------- filters
def _resize(a, w, h, how=Image.BILINEAR):
    if a.ndim == 3:
        return np.stack([_resize(a[..., i], w, h, how) for i in range(a.shape[2])], -1)
    return np.asarray(Image.fromarray(np.ascontiguousarray(a, F32), 'F').resize((w, h), how), F32)


def blur(img, sx, sy=None):
    """Gaussian blur. Large radii are done on a reduced copy for speed."""
    sy = sx if sy is None else sy
    H, W = img.shape[:2]
    k = int(max(1, min(sx if sx > 0.5 else 1e9, sy if sy > 0.5 else 1e9) / 3))
    if k > 1:
        small = _resize(img, max(2, W // k), max(2, H // k), Image.BOX)
        fx = small.shape[1] / W; fy = small.shape[0] / H
    else:
        small, fx, fy = img, 1.0, 1.0
    sig = (sy * fy, sx * fx) + ((0,) if img.ndim == 3 else ())
    small = ndi.gaussian_filter(small, sig, mode='nearest')
    return _resize(small, W, H, Image.BICUBIC) if k > 1 else small


def bloom(img, S, thresh=0.25, radii=(8, 28, 90, 260), gains=(0.30, 0.22, 0.16, 0.10)):
    """Soft multi-scale glow from the bright parts of the image."""
    lum = img.max(-1, keepdims=True)
    hi = img * np.clip((lum - thresh) / (lum + 1e-6), 0, 1)
    out = img.copy()
    for r, g in zip(radii, gains):
        out += g * blur(hi, r * S)
    return out


def sample(img, x, y):
    """Bilinear lookup of img at float pixel coords (x, y), edges clamped."""
    H, W = img.shape[:2]
    c = np.stack([np.clip(y, 0, H - 1), np.clip(x, 0, W - 1)])
    if img.ndim == 2:
        return ndi.map_coordinates(img, c, order=1, mode='nearest')
    return np.stack([ndi.map_coordinates(img[..., i], c, order=1, mode='nearest')
                     for i in range(img.shape[2])], -1)


# ---------------------------------------------------------------- canvas
class Canvas:
    """Pixel grids. px, py in pixels; S is the scale against 2160p."""

    def __init__(self, W, H):
        self.W, self.H = W, H
        self.S = H / 2160.0
        self.py, self.px = np.mgrid[0:H, 0:W].astype(F32)
        self.px += 0.5; self.py += 0.5
        self.u = self.px / H          # 0..16/9
        self.v = self.py / H          # 0..1
        self.aspect = W / H


def stars(cv, density=0.00025, seed=7, size=1.0):
    """Sparse stars with a steep brightness distribution and soft cores."""
    rng = np.random.default_rng(seed)
    n = int(cv.W * cv.H * density / (cv.S * cv.S))
    out = np.zeros((cv.H, cv.W), F32)
    xs = rng.random(n) * cv.W; ys = rng.random(n) * cv.H
    mag = rng.random(n) ** 7
    for x, y, m in zip(xs, ys, mag):
        r = (0.55 + 1.1 * m) * cv.S * size
        R = int(np.ceil(r * 3)) + 1
        x0, y0 = int(x), int(y)
        xa, xb = max(0, x0 - R), min(cv.W, x0 + R + 1)
        ya, yb = max(0, y0 - R), min(cv.H, y0 + R + 1)
        if xa >= xb or ya >= yb:
            continue
        gy, gx = np.mgrid[ya:yb, xa:xb]
        d2 = (gx + 0.5 - x) ** 2 + (gy + 0.5 - y) ** 2
        out[ya:yb, xa:xb] += (0.05 + 0.95 * m) * np.exp(-d2 / (2 * (r * 0.6) ** 2))
    return out


def conifers(u, seed, height, spacing):
    """Silhouette height of a row of conifers: jittered, serrated triangles."""
    rng = np.random.default_rng(seed)
    tab = rng.random((3, 4096)).astype(F32)
    k0 = np.floor(u / spacing).astype(np.int64)
    out = np.zeros_like(u)
    for dk in (-2, -1, 0, 1, 2):
        k = k0 + dk
        j = tab[0, k & 4095]; hh = tab[1, k & 4095]; ww = tab[2, k & 4095]
        uc = (k + 0.5 + 0.8 * (j - 0.5)) * spacing
        Hk = height * (0.35 + 0.65 * hh ** 1.5)
        wk = Hk * (0.20 + 0.10 * ww)                    # spruce proportions: tall and narrow
        a = np.abs(u - uc) / wk
        tri = np.clip(1 - a, -1, 1)
        tri = np.sign(tri) * np.abs(tri) ** 1.12
        # drooping branch tiers, finer towards the top, different on every tree
        serr = 0.045 * (((a * (7 + 5 * j) + 3 * ww) % 1.0) - 0.5) * np.clip(a * 6, 0, 1)
        out = np.maximum(out, Hk * (tri + serr))
    clump = 0.6 + 0.4 * vnoise(u / (spacing * 9), u * 0 + 3.0, seed + 5)
    return np.clip(out, 0, None) * clump


def ridge(cv, seed, base, amp, freq, yh, valley_u, valley_w, valley_depth, rough=0.5, trees=0.0, tree_sp=0.004):
    """Ridge line (v coordinate per column) from 1D fractal noise."""
    u = cv.u[0]
    n = fbm(u * freq, np.full_like(u, seed * 3.7), octaves=7, gain=rough, seed=seed)
    n = n + 0.55 * ridged(u * freq * 0.8, np.full_like(u, 9.1 + seed), octaves=5, seed=seed + 40) - 0.08
    env = 1 - valley_depth * np.exp(-((u - valley_u) / valley_w) ** 2)
    h = base + amp * (n - 0.35) * env
    if trees:
        h = h + conifers(u, seed + 80, trees, tree_sp)
    h = h + 0.0035 * (fbm(u * 55, np.full_like(u, 1.0), 4, seed=seed + 60) - 0.5) + 0.0012 * (vnoise(u * 400, u * 0, seed + 61) - 0.5)
    return yh - h



# ---------------------------------------------------------------- finish
def tonemap(img, shoulder=0.75):
    """Leave darks untouched, roll highlights off smoothly towards 1."""
    x = np.clip(img, 0, None)
    over = np.clip(x - shoulder, 0, None)
    k = 1 - shoulder
    return np.where(x > shoulder, shoulder + k * (1 - np.exp(-over / k)), x).astype(F32)


def finish(img, cv, seed=1, vignette=0.35, grain=0.012, lift=None):
    """Vignette, tone map, film grain. Returns display-referred sRGB float."""
    cx = (cv.u - cv.aspect / 2) / (cv.aspect / 2); cy = (cv.v - 0.5) / 0.5
    r2 = cx * cx * 0.85 + cy * cy
    img = img * (1 - vignette * sstep(0.25, 1.9, r2))[..., None]
    if lift is not None:
        img = np.maximum(img, 0) + lift
    out = lin2srgb(tonemap(np.nan_to_num(img)))
    # film grain: slightly soft, stronger in the midtones, mostly luminance
    rng = np.random.default_rng(seed)
    g = rng.standard_normal((cv.H, cv.W)).astype(F32)
    if cv.S > 0.75:
        g = ndi.gaussian_filter(g, 0.7 * cv.S) * 1.9
    lum = out.mean(-1)
    amp = grain * (0.45 + 0.9 * np.sqrt(np.clip(lum, 0, 1)) * (1 - 0.6 * lum))
    out = out + (g * amp)[..., None]
    out += rng.standard_normal(out.shape).astype(F32) * (grain * 0.12)
    return out


def to8(srgb, seed=3):
    """Quantise to 8 bit with triangular dither so gradients do not band."""
    rng = np.random.default_rng(seed)
    d = rng.random(srgb.shape, dtype=F32) - rng.random(srgb.shape, dtype=F32)
    return np.clip(np.floor(srgb * 255 + 0.5 + d), 0, 255).astype(np.uint8)


def save(srgb, path, also_half=True):
    Image.fromarray(to8(srgb)).save(path, optimize=True)
    print('wrote', path)
    if also_half:
        H, W = srgb.shape[:2]
        half = _resize(srgb, W // 2, H // 2, Image.LANCZOS)
        p2 = path.replace(f'{W}x{H}', f'{W // 2}x{H // 2}')
        Image.fromarray(to8(half, seed=4)).save(p2, optimize=True)
        print('wrote', p2)


def cli(name, render, suffix=''):
    """python3 <script>.py [--preview] [--mode night|day]"""
    ap = argparse.ArgumentParser()
    ap.add_argument('--preview', action='store_true', help='fast 1280x720 draft into preview/')
    ap.add_argument('--mode', default='night', choices=['night', 'day', 'lock'])
    ap.add_argument('--out', default=HERE, help='output directory')
    a = ap.parse_args()
    if a.preview:
        cv = Canvas(1280, 720)
        os.makedirs(os.path.join(HERE, 'preview'), exist_ok=True)
        out = render(cv, a.mode)
        Image.fromarray(to8(out)).save(os.path.join(HERE, 'preview', f'{name}-{a.mode}{suffix}.png'))
        print('wrote preview', name)
    else:
        cv = Canvas(3840, 2160)
        os.makedirs(a.out, exist_ok=True)
        save(render(cv, a.mode), os.path.join(a.out, f'{name}-{a.mode}{suffix}-3840x2160.png'))
