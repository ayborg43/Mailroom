#!/usr/bin/env python3
"""Regenerates the PWA icons in internal/webmail/static/icons/.

The icons are committed, so you only need to run this (it needs Pillow:
`pip install pillow`) when changing the artwork:

    python3 web/scripts/make-icons.py
"""
from pathlib import Path

from PIL import Image, ImageDraw

OUT = Path(__file__).resolve().parents[2] / "internal" / "webmail" / "static" / "icons"
BLUE = (37, 99, 235, 255)  # --accent-solid
WHITE = (255, 255, 255, 255)
CLEAR = (0, 0, 0, 0)
SS = 4  # supersampling factor, for smooth edges


def envelope(px, glyph, background=None, rounded=False):
    """An envelope glyph filling `glyph` (0-1) of a px×px square.

    background: None for a transparent canvas (used for the monochrome badge),
    else BLUE. rounded: round the background's corners (for icons shown
    as-is); leave False for full-bleed icons the OS masks itself.
    """
    s = px * SS
    img = Image.new("RGBA", (s, s), CLEAR)
    d = ImageDraw.Draw(img)
    if background:
        if rounded:
            d.rounded_rectangle([0, 0, s - 1, s - 1], radius=int(s * 0.22), fill=background)
        else:
            d.rectangle([0, 0, s, s], fill=background)

    gw = s * glyph
    gh = gw * 0.72
    x0, y0 = (s - gw) / 2, (s - gh) / 2
    d.rounded_rectangle([x0, y0, x0 + gw, y0 + gh], radius=gh * 0.14, fill=WHITE)

    # The flap: a "V" cut into the body (blue on the app icons, transparent on
    # the badge so only the silhouette remains).
    cut = background if background else CLEAR
    flap = [
        (x0 + gw * 0.07, y0 + gh * 0.14),
        (x0 + gw * 0.50, y0 + gh * 0.58),
        (x0 + gw * 0.93, y0 + gh * 0.14),
    ]
    d.line(flap, fill=cut, width=max(2, int(gh * 0.085)), joint="curve")
    for x, y in (flap[0], flap[2]):  # round the line ends
        r = max(1, int(gh * 0.085)) / 2
        d.ellipse([x - r, y - r, x + r, y + r], fill=cut)

    return img.resize((px, px), Image.LANCZOS)


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    icons = {
        # Shown as-is by browsers/launchers: rounded, transparent corners.
        "icon-192.png": envelope(192, 0.62, BLUE, rounded=True),
        "icon-512.png": envelope(512, 0.62, BLUE, rounded=True),
        "favicon-32.png": envelope(32, 0.70, BLUE, rounded=True),
        # Full-bleed: the OS applies its own mask. Maskable icons must keep
        # the glyph inside the central ~80% "safe zone".
        "icon-maskable-512.png": envelope(512, 0.50, BLUE),
        "apple-touch-icon.png": envelope(180, 0.58, BLUE),
        # Android status-bar badge: alpha-only silhouette.
        "badge-96.png": envelope(96, 0.84),
    }
    for name, img in icons.items():
        img.save(OUT / name, optimize=True)
        print("wrote", OUT / name)


if __name__ == "__main__":
    main()
