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
"""Build the Orpheus brand assets.

    uv run branding/scripts/build.py

Regenerates logo/, exports/ and web/ from the parameters below and the font in fonts/.
"""

from __future__ import annotations

import io
import json
import math
import re
from functools import cache
from pathlib import Path

import resvg_py
import skia
import uharfbuzz as hb
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont

ROOT = Path(__file__).resolve().parent.parent

# Mark: a regular pointy-top hexagon ring cut into four ribbons with round ends.
# Mark units: a 200 x 220 artboard centred on (100, 110).
CX, CY = 100.0, 110.0
OUTER, INNER = 100.0, 41.2  # apothems of the ring and of the hole
OUTER_R, INNER_R = 38.0, 6.0  # corner radii of the ring and of the hole
END_R = (OUTER - INNER) / 2  # round ribbon end: half the ribbon width
MID = (OUTER + INNER) / 2  # apothem of the ribbon centreline
COS30 = math.cos(math.radians(30))
RIGHT_END = (CX + MID, CY + 17.3)  # centre of the right ribbon's round end
WRAP_END = (CX - MID, CY + 28.6)  # original: the lower-left ribbon wraps up to here
FAR = 118.0  # construction points just outside the ring
MARK_TOP = CY - (OUTER - OUTER_R) / COS30 - OUTER_R  # the ring's top edge, 0.41
MARK_W, MARK_H = 2 * OUTER, 2 * (CY - MARK_TOP)

# Linear gradients per ribbon: start, end and three stops, measured from the source
# raster. A is the top-left ribbon, then clockwise B (right), C (bottom), D (lower-left).
GRADIENTS = {
    "a": ((7.9, 113.2), (108.2, 3.1), ("#007b91", "#09bbb4", "#3ff1b6")),
    "b": ((110.4, 65.5), (206.5, 104.3), ("#03b399", "#27dda2", "#43f59f")),
    "c": ((180.5, 135), (84.6, 212.2), ("#027281", "#029b9d", "#05bfae")),
}
D_GRADIENT = {
    "original": ((70.4, 198.6), (17.3, 116.3), ("#008795", "#06a8ab", "#3ad2bc")),
    "straight": ((87, 184), (3.9, 136.1), ("#008795", "#06a8ab", "#3ad2bc")),
}
MARK = "straight"  # the mark: used by the logo, exports and web icons
ALT_MARK = "original"  # as in the source raster, kept for reference

# Mono mark: one colour, each ribbon cut back by a gap around the round end lying on it.
MONO_GAP = 11  # about a fifth of the ribbon width: still visible at 24 px

# Wordmark: set like the agentbox.ru hero heading.
FONT = ROOT / "fonts" / "MartianGroteskSemiExpanded-ExtraBold.woff2"
WORD = "Orpheus"
TRACKING = -0.035  # em
TEXT_SCALE = 0.1  # font units to logo units: cap height 80

# Logo: the mark spans from the descender line to above the ascender.
MARK_TO_CAP = 1.5  # mark height / cap height
GAP = 0.3  # space between the mark and the text / mark height

INK = "#0E0E10"  # text on light backgrounds, background of icons
PAPER = "#EDEDEB"  # text on dark backgrounds


def num(v: float) -> str:
    s = f"{v:.2f}".rstrip("0").rstrip(".")
    return "0" if s in ("", "-0") else s


def pt(p) -> str:
    return f"{num(p[0])} {num(p[1])}"


def polar(r: float, deg: float):
    a = math.radians(deg)
    return CX + r * math.cos(a), CY + r * math.sin(a)


def unit(deg: float):
    a = math.radians(deg)
    return math.cos(a), math.sin(a)


def shift(p, v, k: float):
    return p[0] + k * v[0], p[1] + k * v[1]


def rounded_hexagon(apothem: float, r: float, reverse: bool = False) -> str:
    """Pointy-top hexagon with rounded corners, clockwise unless reversed."""
    corners = [-90, -30, 30, 90, 150, 210]
    step = 30
    if reverse:
        corners.reverse()
        step = -30
    d = ""
    for i, deg in enumerate(corners):
        c = polar((apothem - r) / COS30, deg)
        d += ("M" if i == 0 else "L") + pt(shift(c, unit(deg - step), r))
        d += f"A{num(r)} {num(r)} 0 0 {0 if reverse else 1} {pt(shift(c, unit(deg + step), r))}"
    return d + "Z"


def ribbon(points, end, travel: float, hole_corner=None, stop=None) -> str:
    """Sector of the ring ending in a round end centred on the ribbon centreline.

    points: construction points after the centre, outside the ring or hidden under
    the ribbon drawn above; the ring clip trims everything else. Ribbons run
    clockwise, so their outer side is on the left of the travel direction.
    """
    u = unit(travel)
    outward = (u[1], -u[0])
    d = f"M{pt((CX, CY))}" + "".join(f"L{pt(p)}" for p in points)
    d += f"L{pt(shift(end, outward, END_R))}"
    d += f"A{num(END_R)} {num(END_R)} 0 0 1 {pt(stop or shift(end, outward, -END_R))}"
    if hole_corner:
        d += f"L{pt(hole_corner)}"
    return d + "Z"


def mark_shapes(variant: str) -> dict[str, str]:
    """Ribbon outlines, drawn in the order B, A, D, C, then B's round end.

    Each round end covers the tail of the next ribbon: A over B, B over C, C over D,
    D over A. The cycle is closed by drawing B's body first and its end, a plain
    circle, last.
    """
    mid_v, hole_v = MID / COS30, INNER / COS30
    far = {
        deg: polar(FAR, deg)
        for deg in (-93, -90, -30, 30, 87, 90, 145, 150, 180, 210, 270)
    }
    hole_ll = polar(hole_v, 150)
    chord = RIGHT_END[1]
    shapes = {
        # B: from under A at the top vertex down the right edge; C covers its bottom.
        "b": f"M{pt((CX, CY))}L{pt(far[-93])}L{pt(far[-90])}L{pt(far[-30])}"
        f"L{pt((CX + FAR, chord + 8))}L{pt((CX, chord + 8))}Z",
        "a": ribbon(
            [far[145], far[150], far[210], far[270]],
            polar(mid_v, -90),
            -30,
            polar(hole_v, -90),
        ),
        # C starts exactly at the chord of B's round end.
        "c": ribbon(
            [(CX + INNER, chord), (CX + FAR, chord), far[30], far[90]],
            polar(mid_v, 90),
            150,
            polar(hole_v, 90),
        ),
    }
    if variant == "straight":
        shapes["d"] = ribbon(
            [far[87], far[90], far[150]], polar(mid_v, 150), 210, hole_ll
        )
    else:
        shapes["d"] = ribbon(
            [far[87], far[90], far[150], far[180]],
            WRAP_END,
            -90,
            hole_ll,
            stop=wrap_stop(),
        )
    return shapes


def wrap_stop():
    """Where the wrapped round end meets the hole's bottom-left edge extended past
    the corner: the end sits below that corner, and below the edge it is all D."""
    corner, v = polar(INNER / COS30, 150), unit(210)
    dx, dy = corner[0] - WRAP_END[0], corner[1] - WRAP_END[1]
    b = dx * v[0] + dy * v[1]
    return shift(corner, v, -b - math.sqrt(b * b - dx * dx - dy * dy + END_R**2))


def mark_parts(variant: str, prefix: str) -> tuple[list[str], list[str]]:
    """<defs> content and drawing of the mark in mark units, ids prefixed."""
    shapes = mark_shapes(variant)
    ring = rounded_hexagon(OUTER, OUTER_R) + rounded_hexagon(
        INNER, INNER_R, reverse=True
    )
    defs = [
        f'<clipPath id="{prefix}ring">',
        f'  <path clip-rule="evenodd" d="{ring}"/>',
        "</clipPath>",
    ]
    for key, ((x1, y1), (x2, y2), stops) in {
        **GRADIENTS,
        "d": D_GRADIENT[variant],
    }.items():
        defs.append(
            f'<linearGradient id="{prefix}{key}" x1="{num(x1)}" y1="{num(y1)}" x2="{num(x2)}" y2="{num(y2)}"'
            ' gradientUnits="userSpaceOnUse">'
        )
        defs += [
            f'  <stop offset="{o}" stop-color="{c}"/>'
            for o, c in zip(("0", ".5", "1"), stops)
        ]
        defs.append("</linearGradient>")
    drawing = [f'<g clip-path="url(#{prefix}ring)">']
    drawing += [
        f'  <path fill="url(#{prefix}{key})" d="{shapes[key]}"/>'
        for key in ("b", "a", "d", "c")
    ]
    drawing.append(
        f'  <circle cx="{num(RIGHT_END[0])}" cy="{num(RIGHT_END[1])}" r="{num(END_R)}" fill="url(#{prefix}b)"/>'
    )
    drawing.append("</g>")
    return defs, drawing


def skia_path(d: str) -> skia.Path:
    """Parse the absolute M/L/A/C/Z path data written by this script."""
    path = skia.Path()
    for cmd, args in re.findall(r"([MLACZ])([^MLACZ]*)", d):
        v = [float(n) for n in re.findall(r"-?\d*\.?\d+", args)]
        if cmd == "M":
            path.moveTo(*v)
        elif cmd == "L":
            path.lineTo(*v)
        elif cmd == "C":
            path.cubicTo(*v)
        elif cmd == "A":
            rx, ry, rotation, large, sweep, x, y = v
            size = (
                skia.Path.ArcSize.kLarge_ArcSize
                if large
                else skia.Path.ArcSize.kSmall_ArcSize
            )
            direction = skia.PathDirection.kCW if sweep else skia.PathDirection.kCCW
            path.arcTo(rx, ry, rotation, size, direction, x, y)
        else:
            path.close()
    return path


def grow(path: skia.Path, r: float) -> skia.Path:
    """Dilate a region by r: add a round-joined stroke of its outline."""
    paint = skia.Paint(
        Style=skia.Paint.kStroke_Style,
        StrokeWidth=2 * r,
        StrokeJoin=skia.Paint.kRound_Join,
        StrokeCap=skia.Paint.kRound_Cap,
    )
    band = skia.Path()
    paint.getFillPath(path, band)
    return skia.Op(path, band, skia.kUnion_PathOp)


def contours(path: skia.Path) -> list[skia.Path]:
    out, it = [], skia.Path.Iter(path, False)
    while (step := it.next())[0] != skia.Path.kDone_Verb:
        verb, pts = step
        if verb == skia.Path.kMove_Verb:
            out.append(skia.Path())
            out[-1].moveTo(pts[0])
        elif verb == skia.Path.kLine_Verb:
            out[-1].lineTo(pts[1])
        elif verb == skia.Path.kQuad_Verb:
            out[-1].quadTo(pts[1], pts[2])
        elif verb == skia.Path.kConic_Verb:
            out[-1].conicTo(pts[1], pts[2], it.conicWeight())
        elif verb == skia.Path.kCubic_Verb:
            out[-1].cubicTo(pts[1], pts[2], pts[3])
        else:
            out[-1].close()
    return out


def path_data(path: skia.Path) -> str:
    """M/L/C/Z path data; Skia's conics (circular arcs) and quads become cubics."""
    d, it = "", skia.Path.Iter(path, False)
    while (step := it.next())[0] != skia.Path.kDone_Verb:
        verb, pts = step
        p = [(q.x(), q.y()) for q in pts]
        if verb == skia.Path.kMove_Verb:
            d += f"M{pt(p[0])}"
        elif verb == skia.Path.kLine_Verb:
            d += f"L{pt(p[1])}"
        elif verb in (skia.Path.kQuad_Verb, skia.Path.kConic_Verb):
            w = it.conicWeight() if verb == skia.Path.kConic_Verb else 1
            k = 4 / 3 * w / (1 + w)  # conic -> cubic handles; exact for quads
            c1 = shift(p[0], (p[1][0] - p[0][0], p[1][1] - p[0][1]), k)
            c2 = shift(p[2], (p[1][0] - p[2][0], p[1][1] - p[2][1]), k)
            d += f"C{pt(c1)} {pt(c2)} {pt(p[2])}"
        elif verb == skia.Path.kCubic_Verb:
            d += f"C{pt(p[1])} {pt(p[2])} {pt(p[3])}"
        else:
            d += "Z"
    return d


def mono_path(variant: str = MARK) -> str:
    """Path data of the one-colour mark: one piece per ribbon.

    Each ribbon loses everything within MONO_GAP of the ribbon lying on its tail,
    so the cut follows the round end at radius END_R + MONO_GAP. Covering ribbons
    first run straight across the hole's rounded corners, so the gaps don't hook
    into them.
    """
    ring = skia_path(
        rounded_hexagon(OUTER, OUTER_R) + rounded_hexagon(INNER, INNER_R, reverse=True)
    )
    ring.setFillType(skia.PathFillType.kEvenOdd)
    shapes = mark_shapes(variant)
    hole_v = INNER / COS30
    for key, corner, along in (("a", -90, 150), ("c", 90, -30), ("d", 150, 30)):
        edge = shift(polar(hole_v, corner), unit(along), hole_v / 2)
        shapes[key] = shapes[key][:-1] + f"L{pt(edge)}Z"
    chord = RIGHT_END[1]
    body = skia_path(
        f"M{pt((CX, CY))}L{pt(polar(FAR, -93))}L{pt(polar(FAR, -90))}L{pt(polar(FAR, -30))}"
        f"L{pt((CX + FAR, chord))}L{pt((CX, chord))}Z"
    )
    end = skia.Path()
    end.addCircle(*RIGHT_END, END_R)
    regions = {key: skia_path(shapes[key]) for key in "acd"}
    regions["b"] = skia.Op(body, end, skia.kUnion_PathOp)
    regions = {k: skia.Op(v, ring, skia.kIntersect_PathOp) for k, v in regions.items()}
    mark = skia.Path()
    for key, over in (("a", "d"), ("b", "a"), ("c", "b"), ("d", "c")):
        piece = skia.Op(
            regions[key], grow(regions[over], MONO_GAP), skia.kDifference_PathOp
        )
        mark = skia.Op(mark, piece, skia.kUnion_PathOp)
    # a cut grazing an edge can leave specks: keep the four ribbons
    by_size = sorted(
        contours(mark),
        key=lambda c: c.computeTightBounds().width() * c.computeTightBounds().height(),
    )
    clean = skia.Path()
    for piece in by_size[-4:]:
        clean = skia.Op(clean, piece, skia.kUnion_PathOp)
    return path_data(clean)


def document(view_box, size, body, defs=(), attrs="") -> str:
    root = (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{num(size[0])}" height="{num(size[1])}"'
        f' viewBox="{" ".join(num(v) for v in view_box)}"{attrs} role="img" aria-label="Orpheus">'
    )
    lines = [root]
    if defs:
        lines += ["  <defs>", *(f"    {line}" for line in defs), "  </defs>"]
    lines += [f"  {line}" for line in body]
    return "\n".join([*lines, "</svg>", ""])


def mark_svg(variant: str, prefix: str, view_box=(0, 0, 2 * CX, 2 * CY)) -> str:
    defs, drawing = mark_parts(variant, prefix)
    return document(view_box, view_box[2:], drawing, defs)


def icon_svg(side: int, fraction: float, background: str | None = None) -> str:
    """Square icon: the mark, `fraction` of the side tall, centred on an optional background."""
    s = fraction * side / MARK_H
    x, y = (side - MARK_W * s) / 2, (side - MARK_H * s) / 2 - MARK_TOP * s
    defs, drawing = mark_parts(MARK, "orpheus-icon-")
    body = (
        [f'<rect width="{side}" height="{side}" fill="{background}"/>']
        if background
        else []
    )
    body += [
        f'<g transform="translate({num(x)} {num(y)}) scale({s:.5f})">',
        *(f"  {line}" for line in drawing),
        "</g>",
    ]
    return document((0, 0, side, side), (side, side), body, defs)


@cache
def shaped_word():
    """Glyph names and pen positions of WORD, shaped by HarfBuzz with kerning."""
    font = TTFont(FONT)
    font.flavor = None  # HarfBuzz reads TrueType, not WOFF2
    data = io.BytesIO()
    font.save(data)
    buf = hb.Buffer()
    buf.add_str(WORD)
    buf.guess_segment_properties()
    hb.shape(hb.Font(hb.Face(hb.Blob(data.getvalue()))), buf, {"kern": True})
    names, upm = font.getGlyphOrder(), font["head"].unitsPerEm
    x, placed = 0.0, []
    for info, pos in zip(buf.glyph_infos, buf.glyph_positions):
        placed.append((names[info.codepoint], x + pos.x_offset, pos.y_offset))
        x += pos.x_advance + TRACKING * upm
    return font, tuple(placed)


def outline(x: float, baseline: float):
    """Path data of the wordmark and its ink bounds (x0, y0, x1, y1), y down."""
    font, placed = shaped_word()
    glyphs = font.getGlyphSet()
    pen, bounds = SVGPathPen(glyphs, ntos=num), BoundsPen(glyphs)
    for name, gx, gy in placed:
        t = (
            TEXT_SCALE,
            0,
            0,
            -TEXT_SCALE,
            x + gx * TEXT_SCALE,
            baseline - gy * TEXT_SCALE,
        )
        glyphs[name].draw(TransformPen(pen, t))
        glyphs[name].draw(TransformPen(bounds, t))
    return pen.getCommands(), bounds.bounds


def wordmark_svg() -> str:
    _, (x0, y0, x1, y1) = outline(0, 0)
    d, _ = outline(-x0, -y0)
    return document(
        (0, 0, x1 - x0, y1 - y0),
        (x1 - x0, y1 - y0),
        [f'<path d="{d}"/>'],
        attrs=' fill="currentColor"',
    )


def logo_svg(text_color: str, prefix: str, mark=None) -> tuple[str, float, float]:
    """Mark and wordmark side by side; the cap height is centred on the mark.

    mark: (defs, drawing) in mark units, the colour mark by default.
    """
    font, _ = shaped_word()
    cap = font["OS/2"].sCapHeight * TEXT_SCALE
    mark_h = MARK_TO_CAP * cap
    s = mark_h / MARK_H
    _, (ink_x0, *_) = outline(0, 0)
    d, (_, y0, x1, y1) = outline(MARK_W * s + GAP * mark_h - ink_x0, (mark_h + cap) / 2)
    top, bottom = min(0.0, y0), max(mark_h, y1)
    defs, drawing = mark or mark_parts(MARK, prefix)
    body = [
        f'<g transform="translate(0 {num(-MARK_TOP * s)}) scale({s:.5f})">',
        *(f"  {line}" for line in drawing),
        "</g>",
    ]
    body.append(f'<path fill="{text_color}" d="{d}"/>')
    return (
        document((0, top, x1, bottom - top), (x1, bottom - top), body, defs),
        x1,
        bottom - top,
    )


MANIFEST = {
    "name": "Orpheus",
    "short_name": "Orpheus",
    "icons": [
        {"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
        {"src": "/icon-512.png", "sizes": "512x512", "type": "image/png"},
        {
            "src": "/icon-maskable-512.png",
            "sizes": "512x512",
            "type": "image/png",
            "purpose": "maskable",
        },
    ],
    "theme_color": INK,
    "background_color": INK,
    "display": "standalone",
}

SNIPPET = f"""<!-- Orpheus favicon set: copy branding/web/ to the site root. -->
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="icon" href="/favicon-32.png" sizes="32x32" type="image/png">
<link rel="icon" href="/favicon-16.png" sizes="16x16" type="image/png">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<link rel="manifest" href="/site.webmanifest">
<meta name="theme-color" content="{INK}">

<!--
  favicon.ico is not generated: current browsers use favicon.svg and the PNGs above.
  For legacy browsers, pack favicon-16/32/48.png into /favicon.ico with any ICO tool.
-->
"""


def outputs() -> tuple[dict[str, str], dict[str, tuple[str, int, int]]]:
    """Text files and PNG renders (SVG, width, height), keyed by path under ROOT."""
    dark_logo, _, _ = logo_svg(PAPER, "orpheus-logo-")
    light_logo, w, h = logo_svg(INK, "orpheus-logo-light-")
    favicon = mark_svg(MARK, "orpheus-favicon-", view_box=(CX - CY, 0, 2 * CY, 2 * CY))
    mono = mono_path()
    mono_mark = document(
        (0, 0, 2 * CX, 2 * CY),
        (2 * CX, 2 * CY),
        [f'<path d="{mono}"/>'],
        attrs=' fill="currentColor"',
    )
    mono_dark, _, _ = logo_svg(
        PAPER, "", mark=([], [f'<path fill="{PAPER}" d="{mono}"/>'])
    )
    mono_light, _, _ = logo_svg(
        INK, "", mark=([], [f'<path fill="{INK}" d="{mono}"/>'])
    )
    texts = {
        "logo/orpheus-mark.svg": mark_svg(MARK, "orpheus-mark-"),
        f"logo/orpheus-mark-{ALT_MARK}.svg": mark_svg(
            ALT_MARK, f"orpheus-mark-{ALT_MARK}-"
        ),
        "logo/orpheus-wordmark.svg": wordmark_svg(),
        "logo/orpheus-logo.svg": dark_logo,
        "logo/orpheus-logo-light.svg": light_logo,
        "logo/orpheus-mark-mono.svg": mono_mark,
        "logo/orpheus-logo-mono.svg": mono_dark,
        "logo/orpheus-logo-mono-light.svg": mono_light,
        "web/favicon.svg": favicon,
        "web/site.webmanifest": json.dumps(MANIFEST, indent=2) + "\n",
        "web/favicon-snippet.html": SNIPPET,
    }
    pngs = {"logo/orpheus-logo-email.png": (light_logo, round(96 * w / h), 96)}
    for side in (512, 1024):
        pngs[f"exports/orpheus-mark-{side}.png"] = (
            icon_svg(side, 0.75, INK),
            side,
            side,
        )
        pngs[f"exports/orpheus-mark-{side}-alpha.png"] = (
            icon_svg(side, 0.75),
            side,
            side,
        )
    for side in (16, 32, 48):
        pngs[f"web/favicon-{side}.png"] = (favicon, side, side)
    for name, side, fraction in (
        ("apple-touch-icon", 180, 0.7),
        ("icon-192", 192, 0.75),
        ("icon-512", 512, 0.75),
        ("icon-maskable-512", 512, 0.62),  # inside the maskable safe zone
    ):
        pngs[f"web/{name}.png"] = (icon_svg(side, fraction, INK), side, side)
    return texts, pngs


def main() -> None:
    texts, pngs = outputs()
    for rel, text in texts.items():
        path = ROOT / rel
        path.parent.mkdir(exist_ok=True)
        path.write_text(text, encoding="utf-8")
        print(rel)
    for rel, (svg, width, height) in pngs.items():
        png = resvg_py.svg_to_bytes(svg_string=svg, width=width, height=height)
        (ROOT / rel).write_bytes(bytes(png))
        print(rel)


if __name__ == "__main__":
    main()
