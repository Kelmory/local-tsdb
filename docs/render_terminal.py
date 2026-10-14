"""Render captured terminal text to a PNG 'screenshot' for the README.

This is a docs helper only -- it is not part of the database engine. It turns
the real stdout captured from the demo programs (see ``docs/demo/*.txt``) into
an image that looks like a terminal window, so the README can show genuine
program output without shipping a binary screenshot.

Usage:
    python docs/render_terminal.py docs/demo/gpu_monitor.txt \
        docs/images/gpu_monitor.png "ltsdb - GPU Monitor"
"""
from __future__ import annotations

import re
import sys

from PIL import Image, ImageDraw, ImageFont

ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")

BG = (13, 17, 23)
BAR = (22, 27, 34)
FG = (201, 209, 217)
DIM = (139, 148, 158)
PROMPT = (126, 231, 135)
ACCENT = (121, 192, 255)

# Block-element glyphs are drawn as rectangles instead of glyphs: many monospace
# fonts map them to indistinguishable (or missing) shapes, which would flatten
# the bars and sparklines these dashboards are made of.
BLOCK_FRAC = {"▁": 0.125, "▂": 0.25, "▃": 0.375, "▄": 0.5,
              "▅": 0.625, "▆": 0.75, "▇": 0.875, "█": 1.0}
SHADE = {"░": True, "▒": True, "▓": True}
FILL = (230, 237, 243)
SHADE_FILL = (60, 68, 77)

FONT_CANDIDATES = [
    r"C:\Windows\Fonts\consola.ttf",
    r"C:\Windows\Fonts\cour.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "/System/Library/Fonts/Menlo.ttc",
]


def load_font(size: int) -> ImageFont.FreeTypeFont:
    for path in FONT_CANDIDATES:
        try:
            return ImageFont.truetype(path, size)
        except OSError:
            continue
    return ImageFont.load_default()


def main() -> None:
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    in_path, out_path, title = sys.argv[1], sys.argv[2], sys.argv[3]

    text = open(in_path, encoding="utf-8", errors="replace").read()
    text = ANSI.sub("", text).replace("\t", "    ").rstrip("\n")
    lines = text.split("\n")

    size, pad, bar_h = 15, 18, 34
    line_h = int(size * 1.5)
    font = load_font(size)
    char_w = font.getlength("M")

    cols = max((len(line) for line in lines), default=1)
    width = int(cols * char_w + pad * 2)
    height = bar_h + pad + len(lines) * line_h + pad

    img = Image.new("RGB", (width, height), BG)
    draw = ImageDraw.Draw(img)

    # Title bar with the three traffic-light dots.
    draw.rectangle([0, 0, width, bar_h], fill=BAR)
    for i, colour in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
        cx, cy = 18 + i * 20, bar_h // 2
        draw.ellipse([cx - 6, cy - 6, cx + 6, cy + 6], fill=colour)
    draw.text((width / 2, bar_h / 2), title, font=font, fill=DIM, anchor="mm")

    y = bar_h + pad
    for line in lines:
        colour = FG
        if line.startswith("$ "):
            colour = PROMPT
        elif line.startswith(" GPU Monitor") or line.lstrip().startswith("GPU0"):
            colour = ACCENT

        run, run_start = "", 0
        for i, ch in enumerate(line):
            if ch in BLOCK_FRAC or ch in SHADE:
                if run:
                    draw.text((pad + run_start * char_w, y), run, font=font, fill=colour)
                    run = ""
                x = pad + i * char_w
                if ch in BLOCK_FRAC:
                    top = y + line_h * (1 - BLOCK_FRAC[ch])
                    draw.rectangle([x, top, x + char_w - 0.5, y + line_h - 1], fill=FILL)
                else:
                    draw.rectangle([x, y, x + char_w - 0.5, y + line_h - 1],
                                   fill=SHADE_FILL)
            else:
                if not run:
                    run_start = i
                run += ch
        if run:
            draw.text((pad + run_start * char_w, y), run, font=font, fill=colour)
        y += line_h

    img.save(out_path)
    print("wrote {} ({})".format(out_path, "x".join(map(str, img.size))))


if __name__ == "__main__":
    main()
