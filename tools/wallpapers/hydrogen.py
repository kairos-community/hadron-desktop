#!/usr/bin/env python3
"""Isotope 1 "Hydrogen" ... the water-former.

A single drop has just landed on a still mountain lake before first light.
Run:  python3 hydrogen.py            (4K + 1080p night)
      python3 hydrogen.py --mode day (4K + 1080p day)
      python3 hydrogen.py --preview  (fast draft)
"""
import numpy as np
from lib import *


def render(cv, mode='night'):
    day = mode == 'day'
    lock = mode == 'lock'          # night picture with its light at the centre of the screen
    W, H, S = cv.W, cv.H, cv.S
    u, v = cv.u, cv.v
    yh = 0.515                         # horizon

    # ---------------- ridges first, so the light can sit just behind the far one
    layers = [  # seed, base, amp, freq, valley_w, valley_depth, haze, trees
        (3, 0.085, 0.20, 1.0, 0.45, 0.55, 0.80, 0.0),
        (5, 0.060, 0.22, 1.4, 0.36, 0.80, 0.56, 0.0),
        (8, 0.040, 0.24, 1.9, 0.30, 0.93, 0.30, 0.0),
        (13, 0.020, 0.20, 2.7, 0.36, 0.97, 0.08, 0.007),
    ]
    gu = cv.aspect / 2 if lock else 1.205
    U = cv.u[0]
    ridges = []
    for i, (seed, base, amp, freq, vw, vd, haze, trees) in enumerate(layers):
        # right hand side carries more mass, left stays low and quiet
        side = 0.55 + 0.75 * sstep(gu - 0.1, gu + 0.5, U) + 0.25 * sstep(gu - 0.3, 0.0, U)
        rv = ridge(cv, seed, base, amp, freq, yh, gu - 0.03 * i, vw, vd, trees=trees)
        ridges.append(yh - (yh - rv) * side)
    gv = float(np.interp(gu, U, ridges[0])) + 0.006      # light source, just under the far ridge line
    if lock:                                             # slide the whole scene so the light is at mid height
        dlt = 0.5 - gv
        yh += dlt; gv = 0.5
        ridges = [r + dlt for r in ridges]

    # ---------------- sky
    t = sstep(0.0, yh, v)
    du = (u - gu); dv = (v - gv)
    d = np.sqrt((du / 1.9) ** 2 + dv ** 2)
    d2 = np.sqrt((du / 1.25) ** 2 + dv ** 2)
    if day:
        sky = mix(hexc('CDCEC7'), hexc('F1F2EE'), t ** 1.3)
        sky += (np.exp(-d / 0.30) * 0.22)[..., None] * hexc('F6F0C0')
        air = sky.copy()
        sky += (np.exp(-d2 / 0.07) * 0.45)[..., None] * hexc('F9F1AF')
        sky += (np.exp(-d2 / 0.018) * 1.6)[..., None] * hexc('FFFBE6')
    else:
        sky = mix(PAL['crust'] * 0.85, hexc('37352D'), t ** 1.8)
        band = np.exp(-np.abs(dv) / 0.05) * np.exp(-np.abs(du) / 0.8)
        sky += (np.exp(-d / 0.30) * 0.050)[..., None] * hexc('665F48')       # wide cool air glow
        sky += (band * 0.085)[..., None] * hexc('93854E')                    # dawn line along the horizon
        air = sky.copy()
        sky += (np.exp(-d2 / 0.085) * 0.22)[..., None] * hexc('A08C3C')
        sky += (np.exp(-d2 / 0.036) * 0.55)[..., None] * PAL['brand']
        sky += (np.exp(-d2 / 0.013) * 3.0)[..., None] * PAL['brand_glow']
        # faint rays fanning up from the hidden source
        ang = np.arctan2(-dv, du)
        rays = sstep(0.35, 0.75, fbm(ang * 4.5, np.zeros_like(ang) + 2.0, 2, seed=61))
        sky += (rays * np.exp(-d2 / 0.13) * sstep(0.0, 0.05, -dv) * 0.035)[..., None] * hexc('D6C678')
        st = stars(cv, density=0.00016, seed=11)
        fade = sstep(0.22, 0.02, v) * 0.5 + sstep(yh - 0.02, 0.1, v) * 0.5
        sky += (st * fade * (1 - np.exp(-d * 2.2)) * 0.55)[..., None] * hexc('F0EEE2')

    # ---------------- mountains, far to near, with haze
    hazeimg = blur(air, 70 * S, 40 * S)
    near = hexc('5E5C50') if day else PAL['crust'] * 0.50
    for i, (seed, base, amp, freq, vw, vd, haze, trees) in enumerate(layers):
        rv = ridges[i]
        m = sstep(-0.7 / H, 0.7 / H, v - rv[None, :])
        depth_in = np.clip((v - rv[None, :]) / 0.10, 0, 1)
        fogn = fbm(u * 3.0 + i * 7, v * 22 + i, 4, seed=30 + i)
        fog = np.exp(-(yh - v) / (0.014 + 0.008 * i)) * (0.45 + 0.6 * fogn)
        col = mix(near, hazeimg, haze)
        col = mix(col, hazeimg * 1.05, np.clip(fog, 0, 1) * 0.8)
        # faint slope shading so the layers are not flat cut-outs
        tex = fbm(u * 22 + i * 3, v * 22, 4, seed=50 + i) - 0.5
        col = col * (1 + (0.35 * (1 - haze)) * tex * depth_in)[..., None]
        # light wrapping over the crest near the source
        rim = (0.0 if day else 1.0) * np.exp(-np.clip(v - rv[None, :], 0, 1) / 0.0022) * np.exp(-np.abs(du) / (0.06 + 0.02 * i))
        col = col + (rim * (0.35 if i == 0 else 0.06))[..., None] * PAL['brand']
        sky = mix(sky, col, m)

    # low mist lying on the far water
    mistn = fbm(u * 2.2, v * 40, 4, seed=77)
    mist = np.exp(-np.abs(v - (yh - 0.004)) / 0.010) * (0.30 + 0.8 * mistn)
    sky = mix(sky, hazeimg * 1.15, np.clip(mist, 0, 0.8) * sstep(yh + 0.004, yh - 0.02, v))

    # ---------------- water
    f = 1.15 * H
    yhp = yh * H
    dyp = np.maximum(cv.py - yhp, 0.35)
    tana = dyp / f
    sina = tana / np.sqrt(1 + tana * tana)
    z = 1.0 / tana
    xw = (cv.px - W / 2) / f * z
    foot = z * z / f                    # world metres of depth per pixel

    disp_y = np.zeros((H, W), F32); disp_x = np.zeros((H, W), F32)
    resolved = np.zeros((H, W), F32)
    for i, (lam, amp) in enumerate([(2.6, 0.006), (0.9, 0.006), (0.33, 0.005), (0.12, 0.004)]):
        fade = sstep(1.5, 5.0, lam / foot)
        n = fbm(xw * 0.33 / lam + 11 * i, z / lam + 5 * i, 2, seed=90 + i) - 0.5
        disp_y += 2 * f * amp * 2 * n * fade
        n2 = vnoise(xw * 0.5 / lam + 3 * i, z / lam + 9 * i, seed=95 + i) - 0.5
        disp_x += 2 * f * amp * 2 * n2 * fade
        resolved += fade / 4

    # the drop: concentric capillary rings, finer towards the outside
    x0, z0 = (-0.10 if lock else 0.86), 3.75
    rx, rz = xw - x0, z - z0
    r = np.sqrt(rx * rx + rz * rz) + 1e-4
    lam0 = 0.30
    phase = 2 * np.pi * (r / lam0) ** 1.18
    lam_loc = lam0 * (r / lam0 + 0.3) ** -0.18
    env = sstep(0.0, 0.10, r) * np.exp(-(r / 1.25) ** 2) / np.sqrt(r + 0.25)
    env += 0.9 * np.exp(-(r / 0.16) ** 2)
    slope = 0.105 * np.sin(phase - 0.6) * env * sstep(1.2, 3.0, lam_loc / foot)
    disp_y += 2 * f * slope * rz / r
    disp_x += 2 * f * slope * rx / r

    my = 2 * yhp - cv.py + disp_y
    my = np.minimum(my, yhp - 0.5)
    mx = cv.px + disp_x * sina
    sky_soft = blur(sky, 2.5 * S, 16 * S)
    sky_far = blur(sky, 5 * S, 44 * S)
    refl = mix(sample(sky_far, mx, my), sample(sky_soft, mx, my), resolved[..., None] ** 0.7)

    # glitter path: the core of the light smeared down the water by unresolved wavelets
    glint_src = np.clip(sky - 1.1, 0, None) * 0.35 if day else np.clip(sky - 0.5, 0, None)
    glint = sample(blur(glint_src, 5 * S, 190 * S), mx, np.minimum(2 * yhp - cv.py * 0.55 - yhp * 0.45 + disp_y, yhp - 1))
    spark = fbm(xw * 2.2, z * 7.0, 3, seed=120)
    refl = refl + glint * (0.5 + 2.2 * sstep(0.52, 0.80, spark) * sstep(1.0, 4.0, 0.14 / foot))[..., None] * 1.6
    fres = 0.04 + 0.96 * (1 - sina) ** 3.4
    if day:
        deep = hexc('6B6E62') * 0.60
    else:
        deep = hexc('0D0E0B') * 0.60
    water = refl * fres[..., None] * (0.92 if not day else 0.97) + deep * (1 - fres[..., None])
    # mist on the far water
    wm = np.exp(-(cv.py - yhp) / (0.022 * H)) * (0.35 + 0.5 * fbm(u * 2.2, v * 40, 3, seed=78))
    water = mix(water, sample(hazeimg, cv.px, np.full_like(cv.py, yhp - 6 * S)), np.clip(wm, 0, 0.7))

    wmask = sstep(-0.6, 0.6, cv.py - yhp)
    img = mix(sky, water, wmask)

    # ---------------- the rebound bead above the impact point, and a smaller one above it
    if True:
        cxp = W / 2 + x0 * f / z0; cyp = yhp + f / z0
        for hgt, rad in ((0.165, 0.042), (0.300, 0.020)):
            R = rad * f / z0; by = cyp - hgt * f / z0
            nx = (cv.px - cxp) / R; ny = (cv.py - by) / R
            rr = nx * nx + ny * ny
            inside = sstep(1.0, 1.0 - 2.5 / R, np.sqrt(rr))
            nz = np.sqrt(np.clip(1 - rr, 0, 1))
            # a water bead is a small lens: the bright horizon shows upside down in its lower half
            lens = np.exp(-((ny - 0.42) / 0.20) ** 2) * (1 - 0.5 * nx * nx)
            rimw = (1 - nz) ** 2.5
            body = (deep * 0.6 if not day else hexc('4A4E42'))
            lc = PAL['brand_glow'] if not day else hexc('FBF6D0')
            bead = body + (lens * 1.3)[..., None] * lc + (rimw * (0.55 + 0.45 * -ny))[..., None] * lc * 0.8
            hl = np.exp(-(((nx + 0.30) / 0.14) ** 2 + ((ny + 0.42) / 0.10) ** 2))
            bead = bead + (hl * 0.9)[..., None] * hexc('FFFBE6')
            img = mix(img, bead, inside)
            # its reflection, squashed and broken by the rings
            ry = cyp + hgt * f / z0
            q = np.exp(-(((cv.px + disp_x * 0.4 - cxp) / (R * 1.0)) ** 2 + ((cv.py - disp_y * 0.5 - ry) / (R * 1.5)) ** 2))
            img = img + (q * 0.30)[..., None] * lc * wmask[..., None]

    # ---------------- grade
    if day:
        img = bloom(img, S, thresh=0.75, gains=(0.10, 0.08, 0.06, 0.04))
        out = finish(img, cv, seed=1, vignette=0.16, grain=0.008)
    else:
        img = bloom(img, S, thresh=0.22)
        out = finish(img, cv, seed=1, vignette=0.40, grain=0.011, lift=PAL['crust'] * 0.30)
    return out


if __name__ == '__main__':
    cli('isotope-1-hydrogen', render)
