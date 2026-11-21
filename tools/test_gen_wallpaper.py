#!/usr/bin/env python3
"""Tests for the login screen's soft wallpaper in tools/gen-wallpaper.py
(--blur). Run: python3 tools/test_gen_wallpaper.py"""

import importlib.util
import os
import random
import struct
import tempfile
import unittest
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("gen_wallpaper", os.path.join(HERE, "gen-wallpaper.py"))
gen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gen)


def paeth(a, b, c):
    p = a + b - c
    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
    return a if pa <= pb and pa <= pc else (b if pb <= pc else c)


def encode(rows, w, bpp, filters):
    """A PNG whose row y uses filter filters[y % len(filters)]."""
    raw = bytearray()
    prev = bytes(w * bpp)
    for y, row in enumerate(rows):
        f = filters[y % len(filters)]
        out = bytearray()
        for i, x in enumerate(row):
            a = row[i - bpp] if i >= bpp else 0
            b = prev[i]
            c = prev[i - bpp] if i >= bpp else 0
            pred = (0, a, b, (a + b) >> 1, paeth(a, b, c))[f]
            out.append((x - pred) & 0xFF)
        raw += bytes([f]) + out
        prev = row

    def chunk(t, d):
        c = struct.pack(">I", len(d)) + t + d
        return c + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    ctype = 2 if bpp == 3 else 6
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, len(rows), 8, ctype, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw))) + chunk(b"IEND", b""))


class ReadPng(unittest.TestCase):
    def roundtrip(self, bpp):
        rnd = random.Random(7)
        w, h = 13, 10
        rows = [bytes(rnd.randrange(256) for _ in range(w * bpp)) for _ in range(h)]
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "x.png")
            with open(p, "wb") as f:
                f.write(encode(rows, w, bpp, [0, 1, 2, 3, 4]))
            rw, rh, got = gen.read_png(p)
        self.assertEqual((rw, rh), (w, h))
        want = rows if bpp == 3 else [bytes(b for i, b in enumerate(r) if i % 4 != 3) for r in rows]
        self.assertEqual(got, want)

    def test_every_filter_rgb(self):
        self.roundtrip(3)

    def test_every_filter_rgba_drops_alpha(self):
        self.roundtrip(4)

    def test_reads_what_write_png_writes(self):
        with open(gen.PALETTE, "rb") as f:
            pal = gen.tomllib.load(f)
        rows = gen.draw(pal["night"], "night", 64, 36, True)
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "x.png")
            gen.write_png(p, 64, 36, rows)
            self.assertEqual(gen.read_png(p), (64, 36, rows))


class Blur(unittest.TestCase):
    def test_quarter_size_and_flat_stays_flat(self):
        w, h = 320, 180
        rows = [bytes([40, 50, 60]) * w for _ in range(h)]
        ow, oh, out = gen.blur_rows(w, h, rows)
        self.assertEqual((ow, oh), (80, 45))
        vals = {v for r in out for v in r[0::3]}
        self.assertTrue(vals <= {39, 40, 41}, vals)

    def test_a_hard_edge_becomes_a_ramp(self):
        # Half black, half white. Sigma scales with the height: at 960x540,
        # a quarter of 3840x2160, it is 10 source pixels, 2.5 output pixels.
        w, h = 960, 540
        rows = [bytes([0, 0, 0]) * (w // 2) + bytes([255, 255, 255]) * (w // 2) for _ in range(h)]
        ow, _, out = gen.blur_rows(w, h, rows)
        line = out[len(out) // 2][0::3]
        mid = ow // 2
        self.assertLess(line[0], 3)
        self.assertGreater(line[-1], 252)
        self.assertTrue(70 < line[mid - 1] < 128 < line[mid] < 185, list(line[mid - 6:mid + 6]))
        # The ramp is about four sigma wide.
        steps = sum(1 for x in range(ow) if 5 < line[x] < 250)
        self.assertTrue(6 < steps < 20, steps)

    def test_deterministic(self):
        rnd = random.Random(3)
        rows = [bytes(rnd.randrange(256) for _ in range(160 * 3)) for _ in range(90)]
        self.assertEqual(gen.blur_rows(160, 90, rows), gen.blur_rows(160, 90, rows))


if __name__ == "__main__":
    unittest.main()
