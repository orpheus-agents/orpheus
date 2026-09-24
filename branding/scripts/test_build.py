#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "fonttools[woff]>=4.66",
#     "uharfbuzz>=0.56",
#     "resvg-py>=0.5",
#     "skia-python==144.0.post2",
# ]
# ///
"""Checks for build.py:  uv run branding/scripts/test_build.py"""

from __future__ import annotations

import json
import math
import re
import struct
import sys
import unittest
import xml.etree.ElementTree as ET
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import build


class BuildTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.texts, cls.pngs = build.outputs()

    def test_committed_files_match_the_script(self):
        for rel, text in self.texts.items():
            with self.subTest(rel):
                committed = (build.ROOT / rel).read_text(encoding="utf-8")
                self.assertEqual(committed, text, "run branding/scripts/build.py")

    def test_pngs_have_declared_sizes(self):
        for rel, (_, width, height) in self.pngs.items():
            with self.subTest(rel):
                header = (build.ROOT / rel).read_bytes()[:24]
                self.assertEqual(header[:8], b"\x89PNG\r\n\x1a\n")
                self.assertEqual(struct.unpack(">II", header[16:24]), (width, height))

    def test_manifest_icons_exist_in_declared_sizes(self):
        for icon in json.loads(self.texts["web/site.webmanifest"])["icons"]:
            with self.subTest(icon["src"]):
                size = tuple(map(int, icon["sizes"].split("x")))
                self.assertEqual(self.pngs["web" + icon["src"]][1:], size)

    def test_ids_are_unique_across_svgs_and_references_resolve(self):
        # Pages inline the light and dark logos together, so ids must not clash.
        owner = {}
        for rel, text in self.texts.items():
            if not rel.endswith(".svg"):
                continue
            ids = [el.get("id") for el in ET.fromstring(text).iter() if el.get("id")]
            for id_ in ids:
                self.assertNotIn(id_, owner, f"{rel} reuses an id of {owner.get(id_)}")
                owner[id_] = rel
            refs = set(re.findall(r"url\(#([^)]+)\)", text))
            self.assertLessEqual(refs, set(ids), rel)

    def test_wrapped_end_stops_on_the_hole_edge_at_its_corner(self):
        stop = build.wrap_stop()
        self.assertAlmostEqual(math.dist(stop, build.WRAP_END), build.END_R)
        n = build.unit(120)  # normal of the hole's bottom-left edge
        offset = n[0] * (stop[0] - build.CX) + n[1] * (stop[1] - build.CY)
        self.assertAlmostEqual(offset, build.INNER)
        corner = build.polar(build.INNER / build.COS30, 150)
        self.assertLess(math.dist(stop, corner), 1)

    def test_mono_mark_is_one_piece_per_ribbon_inside_the_colour_mark(self):
        svg = ET.fromstring(self.texts["logo/orpheus-mark-mono.svg"])
        d = svg.find("{http://www.w3.org/2000/svg}path").get("d")
        self.assertEqual(d.count("M"), 4)  # no specks, no merged ribbons
        mono = build.skia_path(d)
        ring = build.skia_path(
            build.rounded_hexagon(build.OUTER + 1, build.OUTER_R)
            + build.rounded_hexagon(build.INNER - 1, build.INNER_R, reverse=True)
        )
        ring.setFillType(build.skia.PathFillType.kEvenOdd)
        outside = build.skia.Op(mono, ring, build.skia.kDifference_PathOp)
        self.assertTrue(outside.isEmpty(), "mono mark leaves the ring")

    def test_logo_mark_spans_from_descender_to_above_ascender(self):
        font, _ = build.shaped_word()
        cap = font["OS/2"].sCapHeight * build.TEXT_SCALE
        mark_h = build.MARK_TO_CAP * cap
        _, (_, top, _, bottom) = build.outline(0, (mark_h + cap) / 2)
        self.assertGreater(top, 0)
        self.assertAlmostEqual(bottom, mark_h)
        svg = self.texts["logo/orpheus-logo-light.svg"]
        view_box = [float(v) for v in ET.fromstring(svg).get("viewBox").split()]
        self.assertEqual(view_box[1], 0)
        self.assertAlmostEqual(view_box[3], mark_h)


if __name__ == "__main__":
    unittest.main()
