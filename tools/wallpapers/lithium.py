#!/usr/bin/env python3
"""Isotope 3 "Lithium" ... the stone.

A salt flat after rain at the last light. The crust has dried into polygons
whose ridges catch the light; a film of brine in the cells mirrors the sky.
Run:  python3 lithium.py   (4K + 1080p night)   |   --preview for a draft
"""
import numpy as np
from lib import *

E2X, E2Z = 0.5, 0.8660254


def salt_cells(x, z, jitter=0.30, seed=300):
    """Voronoi cells on a jittered hexagonal lattice, like desiccation polygons.

    Returns distance to the nearest cell edge, the unit direction away from
    that edge, and a random number per cell.
    """
    rng = np.random.default_rng(seed)
    tab = rng.random((3, 256, 256)).astype(F32)
    b = z / E2Z; a = x - E2X * b
    ia = np.floor(a).astype(np.int64); ib = np.floor(b).astype(np.int64)
    best = np.full(x.shape, 1e9, F32)
    s1x = np.zeros_like(x); s1z = np.zeros_like(x); cid = np.zeros_like(x)
    sites = []
    for dj in (-1, 0, 1, 2):
        for di in (-1, 0, 1, 2):
            i = ia + di; j = ib + dj
            jx = (tab[0, j & 255, i & 255] - 0.5) * 2 * jitter
            jz = (tab[1, j & 255, i & 255] - 0.5) * 2 * jitter
            sx = (i + E2X * j + jx).astype(F32); sz = (j * E2Z + jz).astype(F32)
            d2 = (x - sx) ** 2 + (z - sz) ** 2
            sites.append((sx, sz))
            m = d2 < best
            best = np.where(m, d2, best)
            s1x = np.where(m, sx, s1x); s1z = np.where(m, sz, s1z)
            cid = np.where(m, tab[2, j & 255, i & 255], cid)
    edge = np.full(x.shape, 1e9, F32); gx = np.zeros_like(x); gz = np.zeros_like(x)
    for sx, sz in sites:
        ex = s1x - sx; ez = s1z - sz
        ln = np.sqrt(ex * ex + ez * ez)
        ok = ln > 1e-4
        ln = np.where(ok, ln, 1)
        ex /= ln; ez /= ln
        d = (x - (s1x + sx) * 0.5) * ex + (z - (s1z + sz) * 0.5) * ez
        m = ok & (d < edge)
        edge = np.where(m, d, edge); gx = np.where(m, ex, gx); gz = np.where(m, ez, gz)
    return edge, gx, gz, cid


def render(cv, mode='night'):
    W, H, S = cv.W, cv.H, cv.S
    u, v = cv.u, cv.v
    yh = 0.43
    gu = 1.22                                  # where the last light sits on the horizon

    # ---------------- sky: violet above, a long luminous line at the horizon
    t = sstep(0.0, yh, v)
    sky = mix(PAL['crust'] * 0.85, hexc('322F28'), t ** 1.6)
    du = u - gu; dv = v - yh
    band_w = np.exp(-(du / 0.95) ** 2)
    sky += (np.exp(-np.abs(dv) / 0.16) * (0.35 + 0.65 * band_w) * 0.060)[..., None] * hexc('665F48')
    sky += (np.exp(-np.abs(dv) / 0.055) * band_w * 0.10)[..., None] * hexc('A88A40')
    sky += (np.exp(-np.abs(dv) / 0.020) * np.exp(-(du / 0.48) ** 2) * 0.45)[..., None] * PAL['brand']
    sky += (np.exp(-np.abs(dv) / 0.007) * np.exp(-(du / 0.30) ** 2) * 1.6)[..., None] * PAL['brand_glow']
    air = blur(sky, 80 * S, 30 * S)

    sky_ns = None
    st = stars(cv, density=0.00030, seed=31)
    st[:int(0.05 * H)] *= sstep(0.0, 0.05, v[:int(0.05 * H)])
    sfade = sstep(yh - 0.03, 0.12, v) * (0.30 + 0.70 * sstep(0.3, 1.3, u))
    starl = (st * sfade * 0.65)[..., None] * hexc('F0EEE2')
    sky += starl

    # thin strata, lit from underneath
    cn = fbm(u * 0.9 + 7, v * 34 + 0.9 * fbm(u * 1.3, v * 4, 3, seed=305), 5, seed=321)
    cl = sstep(0.60, 0.85, cn) * sstep(yh - 0.20, yh - 0.10, v) * sstep(yh - 0.012, yh - 0.05, v) * sstep(0.5, 1.0, u)
    sky = sky * (1 - 0.45 * cl[..., None]) + (cl * np.exp(-(du / 0.5) ** 2) * 0.10)[..., None] * PAL['brand_glow']

    # far range on the horizon, deep in haze
    for i, (seed, amp, freq, haze) in enumerate(((331, 0.13, 1.1, 0.78), (337, 0.085, 1.9, 0.52))):
        rv = ridge(cv, seed, 0.004, amp, freq, yh, gu - 0.05, 0.36, 0.90)
        rv = yh - (yh - rv) * (0.45 + 0.8 * sstep(gu, gu + 0.6, cv.u[0]) + 0.35 * sstep(gu - 0.5, 0.0, cv.u[0]))
        if i == 0:   # an old volcano on the right, as on the altiplano
            rv = rv - 0.085 * np.clip(1 - np.abs(cv.u[0] - 1.56) / 0.26, 0, 1) ** 1.7
        m = sstep(-0.7 / H, 0.7 / H, v - rv[None, :])
        col = mix(PAL['crust'] * 0.5, air, haze)
        sky = mix(sky, col, m)

    # ---------------- ground
    f = 1.0 * H
    camh = 0.72
    yhp = yh * H
    rows = slice(int(yhp) - 1, H)
    py = cv.py[rows]; px = cv.px[rows]
    dyp = np.maximum(py - yhp, 0.4)
    tana = dyp / f
    sina = tana / np.sqrt(1 + tana * tana)
    z = camh / tana
    xw = (px - W * 0.60) / f * z                 # vanishing point right of centre, under the light
    foot = z * z / (camh * f)                    # depth covered by one pixel
    footx = z / f

    cs, wr, hr = 0.80, 0.030, 0.030              # cell size, ridge half width, ridge height

    def height(x, zz):
        """Crust height: raised polygon rims with a soft shoulder, crumbly on top."""
        # low frequency warp so no two cells are alike and no edge is ruler straight
        wx = x + 0.55 * (fbm(x * 0.33, zz * 0.33, 3, seed=360) - 0.5) + 0.05 * (fbm(x * 2.3, zz * 2.3, 2, seed=362) - 0.5)
        wz = zz + 0.55 * (fbm(x * 0.33 + 9, zz * 0.33, 3, seed=361) - 0.5) + 0.05 * (fbm(x * 2.3 + 4, zz * 2.3, 2, seed=363) - 0.5)
        e, _, _, cid = salt_cells(wx / cs + 3.3, wz / cs + 1.7, jitter=0.36)
        e = np.maximum(e, 0) * cs
        aa = np.sqrt(foot * footx) * 0.9 + foot * 0.25
        we = np.sqrt(wr * wr + aa * aa)
        along = fbm(x * 3.1, zz * 3.1, 3, seed=340)
        rid = np.exp(-(e / we) ** 2) * (wr / we) * (0.45 + 1.0 * along)
        sh = np.exp(-(e / (4.0 * wr + aa)) ** 1.5)
        d1 = sstep(1.0, 4.0, 0.07 / foot); d2 = sstep(1.0, 4.0, 0.018 / foot)
        bumps = (fbm(x * 14, zz * 14, 4, seed=341) - 0.5) * d1 + 0.40 * (fbm(x * 60, zz * 60, 3, seed=342) - 0.5) * d2
        h = hr * (rid + 0.40 * sh) + 0.012 * bumps * (0.25 + sh)
        return h, cid

    h0, cid = height(xw, z)
    eps = np.maximum(0.004, 0.8 * np.sqrt(foot * footx))
    hx, _ = height(xw + eps, z)
    hz_, _ = height(xw, z + eps)
    nx = -(hx - h0) / eps; nz = -(hz_ - h0) / eps
    del hx, hz_
    nl = np.sqrt(nx * nx + nz * nz + 1)
    nx /= nl; nz /= nl; ny = 1 / nl

    # deeper brine drowns the crust: open mirror on the left and far out, polygons on the right
    lvl = fbm(xw * 0.10 + 2, z * 0.10, 3, seed=370) + 0.42 * sstep(-0.2, -2.6, xw) + 0.20 * sstep(5.0, 18.0, z) - 0.10 * sstep(2.5, 0.8, z)
    wl = hr * (0.20 + 0.10 * cid + 2.0 * sstep(0.54, 0.72, lvl))
    soft = 0.0012 + 0.6 * foot * 0.02
    water = sstep(-soft, soft, wl - h0)
    above = np.clip(h0 - wl, 0, None)

    # light comes low from the horizon, slightly from the right
    L = np.array([0.30, 0.10, 1.0]); L = L / np.linalg.norm(L)
    diff = np.clip(nx * L[0] + ny * L[1] + nz * L[2], 0, None)
    amb = 0.5 + 0.5 * ny
    salt = hexc('DAD9D2')
    lightc = hexc('F6F1D6')
    near_light = np.exp(-((px / H - gu) / 0.75) ** 2)
    reach = (0.10 + 0.90 * near_light) * (0.16 + 0.84 * sstep(0.85, 0.12, sina / 0.5))
    damp = 0.35 + 0.65 * sstep(0.0, 0.006, above)          # salt is darker where it is still wet
    spark = sstep(0.93, 1.0, vnoise(xw * 300, z * 300, 347)) * sstep(1.0, 3.0, 0.0035 / foot)
    alb = 0.70 + 0.55 * fbm(xw * 6, z * 6, 4, seed=348)
    lit = np.clip((nx * L[0] + ny * 0.035 + nz * L[2]) / 0.55, 0, 1) ** 0.85
    diff = 0.35 * diff + 0.75 * lit
    ridge_col = (hexc('6C695A') * salt)[None, None, :] * (amb * 0.030 * damp)[..., None] \
        + salt * ((diff * 0.85 + spark * diff * 2.5) * reach * damp * alb)[..., None] * mix(lightc, PAL['brand_glow'], 0.35 * near_light)

    # brine film: a mirror of the sky, barely disturbed
    wob = (fbm(xw * 1.3, z * 1.3, 3, seed=350) - 0.5) * sstep(1.5, 5, 0.8 / foot)
    my = np.minimum(2 * yhp - py + 2 * f * 0.006 * wob, yhp - 0.5)
    mx = px + (fbm(xw * 1.3 + 5, z * 1.3, 2, seed=351) - 0.5) * 6 * S * sina
    refl = sample(blur(sky - 0.92 * starl, 1.5 * S, 5 * S), mx, my)
    fres = 0.05 + 0.95 * (1 - sina) ** 3.0
    depth = np.clip(wl - h0, 0, None)
    bed = (hexc('79766A') * salt) * (0.016 * np.exp(-depth / 0.012) * (0.7 + 0.6 * fbm(xw * 5, z * 5, 4, seed=355)))[..., None]
    film = refl * fres[..., None] + bed * (1 - fres)[..., None]
    ground = mix(ridge_col, film, water)

    # distance haze
    hz = np.exp(-dyp / (0.030 * H))
    ground = mix(ground, sample(air, px, np.full_like(py, yhp - 4 * S)), hz * 0.75)

    img = sky.copy()
    wmask = sstep(-0.6, 0.6, py - yhp)
    img[rows] = mix(sky[rows], ground, wmask)

    img = bloom(img, S, thresh=0.22)
    return finish(img, cv, seed=3, vignette=0.42, grain=0.011, lift=PAL['crust'] * 0.30)


if __name__ == '__main__':
    cli('isotope-3-lithium', render)
