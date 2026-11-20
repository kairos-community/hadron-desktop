#!/usr/bin/env python3
"""Isotope 2 "Helium" ... first seen in the light of the eclipsed Sun, 1868.

Totality over a sea of fog and ridges. A thin warm arc on the limb is the
chromosphere, where the yellow helium line was first recorded.
Run:  python3 helium.py   (4K + 1080p night)   |   --preview for a draft
"""
import numpy as np
from lib import *


def render(cv, mode='night'):
    W, H, S = cv.W, cv.H, cv.S
    u, v = cv.u, cv.v
    eu, ev, R = 1.185, 0.335, 0.058          # eclipse centre and lunar radius (in screen heights)
    yh = 0.80

    # ---------------- sky
    t = sstep(0.0, yh, v)
    sky = mix(PAL['crust'] * 0.80, hexc('2F2D28'), t ** 1.7)
    # the 360 degree twilight ring seen during totality, kept cool and low
    sky += (np.exp(-np.abs(v - yh) / 0.10) * 0.050)[..., None] * hexc('665F48')
    sky += (np.exp(-np.abs(v - yh) / 0.035) * 0.040)[..., None] * hexc('93854E')

    dx = (u - eu) / R; dy = (v - ev) / R
    r = np.sqrt(dx * dx + dy * dy) + 1e-5
    th = np.arctan2(dy, dx)
    lr = np.log(r)
    c, s_ = np.cos(th), np.sin(th)

    # broad helmet streamers along a tilted equator, finer plumes at the poles
    tilt = np.deg2rad(-24)
    eq = np.cos(th - tilt) ** 2
    big = fbm(c * 1.25 + 4.0 + 0.10 * lr, s_ * 1.25 + 1.5, 3, seed=201)
    big = sstep(0.30, 0.75, big) * (0.35 + 0.65 * eq)
    warp = 0.25 * (fbm(c * 2 + lr * 0.6, s_ * 2, 2, seed=205) - 0.5)
    fine = fbm(c * 7 + warp * 3 + 9, s_ * 7 + 0.25 * lr, 3, seed=210)
    fine2 = fbm(c * 21 + warp * 8, s_ * 21 + 0.35 * lr + 3, 3, seed=215)
    rays = 0.30 + 1.1 * sstep(0.25, 0.85, fine) * (0.55 + 0.9 * fine2)
    p = 3.5 - 1.35 * big                                  # streamers fall off more slowly
    rr = np.maximum(r, 1.0)
    corona = rr ** (-p) * rays * (0.45 + 0.9 * big) * np.exp(-(rr - 1) / 8.0)
    limb = sstep(-0.1, 0.9, np.cos(th - np.deg2rad(150)))
    inner = (np.exp(-(rr - 1) / 0.10) * 1.6 + np.exp(-(rr - 1) / 0.03) * 3.0) * (1 - 0.75 * limb)   # bright inner corona
    halo = np.exp(-(rr - 1) / 2.8) * 0.030 + np.exp(-(rr - 1) / 7.0) * 0.012  # scattered sky glow

    white = hexc('FDF6CC'); limew = PAL['brand_glow']
    colmix = sstep(1.0, 4.5, rr)
    ccol = mix(white, mix(limew, hexc('EBCF78'), sstep(2.0, 9.0, rr)[..., None]), colmix)
    sky += (corona * 0.95)[..., None] * ccol + inner[..., None] * white * 0.9 \
        + halo[..., None] * mix(PAL['brand'], hexc('9A7E4A'), 0.7)

    # chromosphere: a thin helium-yellow arc and two small prominences on one limb
    he = hexc('FFB23A')
    arc = np.exp(-np.abs(r - 1.012) / 0.014) * limb
    sky += (arc * 6.5)[..., None] * he
    for ang, ln, wd in ((128, 0.085, 0.045), (166, 0.055, 0.032)):
        a = np.deg2rad(ang)
        along = (dx * np.cos(a) + dy * np.sin(a)) - 1.0
        across = (-dx * np.sin(a) + dy * np.cos(a)) + 0.35 * along   # leaning, like a flame
        pr = np.exp(-(across / (wd * (1 - np.clip(along / (3 * ln), 0, 0.8)))) ** 2) \
            * sstep(-0.012, 0.004, along) * np.exp(-np.clip(along, 0, 9) / ln)
        sky += (pr * 4.5)[..., None] * mix(he, hexc('FF7A3A'), 0.45)

    st = stars(cv, density=0.00022, seed=23)
    sfade = sstep(yh - 0.05, 0.25, v) * sstep(2.0, 7.0, r) * (0.35 + 0.65 * sstep(0.2, 1.2, u))
    sky += (st * sfade * 0.6)[..., None] * hexc('F0EEE2')

    # the Moon: not pure black, a breath of earthshine
    disc = sstep(1.0 + 1.2 / (R * H), 1.0 - 1.2 / (R * H), r)
    moon = PAL['crust'] * ((0.42 + 0.18 * fbm(dx * 2.5 + 3, dy * 2.5, 4, seed=230)) * (1.0 - 0.35 * np.minimum(r, 1)))[..., None]
    sky = mix(sky, moon, disc)

    # ---------------- land: ridges standing in a sea of fog, lit from above
    air = blur(sky, 90 * S, 60 * S)
    hazecol = sample(air, cv.px, np.full_like(cv.py, (yh - 0.03) * H))
    pool = np.exp(-((u - eu) / 0.50) ** 2)                # light pooling under the eclipse
    fogc = hexc('3A3830') * (0.85 + 1.5 * pool)[..., None] + (pool * 0.05)[..., None] * limew
    near = PAL['crust'] * 0.40
    img = sky
    layers = [  # seed, crest v, amp, freq, haze, fog level v, trees
        (31, 0.790, 0.085, 1.2, 0.84, 0.800, 0.0),
        (37, 0.825, 0.115, 1.6, 0.64, 0.838, 0.0),
        (41, 0.868, 0.135, 2.1, 0.42, 0.884, 0.0),
        (47, 0.918, 0.140, 2.6, 0.20, 0.940, 0.007),
        (53, 0.985, 0.070, 1.6, 0.00, 1.100, 0.040),
        (59, 1.010, 0.060, 1.1, 0.00, 1.100, 0.085),
    ]
    for i, (seed, cv0, amp, freq, haze, flev, trees) in enumerate(layers):
        rv = ridge(cv, seed, 0.0, amp, freq, cv0, eu - 0.30, 0.45, 0.55, trees=trees, tree_sp=(0.004, 0.004, 0.004, 0.004, 0.013, 0.027)[i])
        # more mass on the right, low on the left
        rv = rv + 0.022 * sstep(1.1, 0.0, cv.u[0]) - 0.020 * sstep(1.25, 1.78, cv.u[0])
        if i == 5:   # the nearest stand of trees only comes into frame on the right
            rv = rv + 0.10 * sstep(1.30, 0.75, cv.u[0]) - 0.035 * sstep(1.30, 1.78, cv.u[0])
        m = sstep(-0.7 / H, 0.7 / H, v - rv[None, :])
        below = np.clip(v - rv[None, :], 0, 1)
        col = mix(near, hazecol * 0.85, haze)
        tex = fbm(u * 26 + i * 3, v * 26, 4, seed=80 + i) - 0.5
        col = col * (1 + 0.30 * (1 - haze) * tex)[..., None]
        # soft top light on the crest from the corona above
        rim = np.exp(-below / 0.0016) * np.exp(-((u - eu) / 0.5) ** 2)
        col = col + (rim * (0.022 if i < 3 else 0.0) * (1 - 0.5 * haze))[..., None] * limew
        img = mix(img, col, m)
        # fog lies at one level in each valley, with a torn upper edge
        fn = fbm(u * 1.9 + i * 5, v * 9 + i * 3, 4, seed=60 + i)
        wisp = fbm(u * 6 + i, v * 30 + 2 * fn, 3, seed=65 + i)
        fog = sstep(-0.016, 0.022, v - flev + 0.030 * (fn - 0.5) + 0.010 * (wisp - 0.5))
        fog *= sstep(-0.002, 0.012, v - np.minimum(rv[None, :], flev - 0.02) )
        top = np.exp(-np.abs(v - flev) / 0.02)
        fc = fogc * (0.80 + 0.45 * top * pool - 0.07 * i)[..., None]
        img = mix(img, fc, fog * (0.95 - 0.08 * i))

    img = bloom(img, S, thresh=0.30, radii=(6, 22, 70, 220), gains=(0.35, 0.25, 0.16, 0.10))
    img = img * (1 - 0.55 * disc)[..., None]              # keep the lunar disc dark under the bloom
    return finish(img, cv, seed=2, vignette=0.40, grain=0.011, lift=PAL['crust'] * 0.30)


if __name__ == '__main__':
    cli('isotope-2-helium', render)
