#!/usr/bin/env python3
"""Tests for the COSMIC theme in tools/gen-theme.py. Run: python3 tools/test_gen_theme.py"""

import importlib.util
import json
import os
import re
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("gen_theme", os.path.join(HERE, "gen-theme.py"))
gen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gen)

PAL = gen.load_palette()


def lums(mode):
    pal = gen.cosmic_theme(PAL)[mode]["palette"]
    return [gen.luminance(pal[f"neutral_{i}"]) for i in range(11)]


class CosmicTheme(unittest.TestCase):
    def test_cosmic_theme_accent_is_palette_accent(self):
        t = gen.cosmic_theme(PAL)
        self.assertEqual(t["dark"]["builder"]["accent"], PAL["night"]["accent"])
        self.assertEqual(t["light"]["builder"]["accent"], PAL["day"]["accent"])

    def test_cosmic_neutral_ramp_is_monotonic(self):
        dark, light = lums("dark"), lums("light")
        self.assertTrue(all(a < b for a, b in zip(dark, dark[1:])), dark)
        self.assertTrue(all(a > b for a, b in zip(light, light[1:])), light)

    def test_cosmic_theme_has_no_foreign_colour(self):
        values = set(PAL["night"].values()) | set(PAL["day"].values())
        allowed = values | {gen.mix(a, b) for a in values for b in values}
        found = re.findall(r"#[0-9a-f]{6}", gen.cosmic_theme_json(PAL))
        self.assertTrue(found)
        self.assertEqual([c for c in found if c not in allowed], [])

    def test_cosmic_theme_on_accent(self):
        t = gen.cosmic_theme(PAL)
        self.assertEqual(t["dark"]["on_accent"], PAL["night"]["on_accent"])
        self.assertEqual(t["light"]["on_accent"], PAL["day"]["on_accent"])

    def test_cosmic_theme_json_is_sorted(self):
        text = gen.cosmic_theme_json(PAL)
        self.assertEqual(text, json.dumps(json.loads(text), indent=2, sort_keys=True) + "\n")

    def test_focused_window_has_a_thin_neutral_hint(self):
        # The spec's fallback: a thin outline in a neutral role, not the accent.
        t = gen.cosmic_theme(PAL)
        for mode, pal in (("dark", PAL["night"]), ("light", PAL["day"])):
            self.assertIn(t[mode]["look"]["active_hint"], (1, 2))
            self.assertEqual(t[mode]["builder"]["window_hint"], pal["dim"])
            self.assertNotEqual(t[mode]["builder"]["window_hint"], pal["accent"])

    def test_mix_is_the_rounded_midpoint(self):
        self.assertEqual(gen.mix("#000000", "#010101"), "#010101")
        self.assertEqual(gen.mix("#102030", "#304050"), "#203040")


if __name__ == "__main__":
    unittest.main()
