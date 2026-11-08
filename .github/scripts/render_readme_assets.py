"""Render the illustrative local-calculation flow used in the README."""

from dataclasses import dataclass
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
ASSETS = ROOT / "docs" / "assets"
SIZE = (720, 560)
BACKGROUND = "#0b1220"
TEXT = "#f1f5f9"
MUTED = "#a5b4c8"
ACCENT = "#5eead4"


@dataclass(frozen=True)
class TextStyle:
    """Font size, color and width constraint for a figure label."""

    size: int
    color: str = TEXT
    max_width: int = 544


def font(size: int) -> ImageFont.FreeTypeFont:
    """Use an available TrueType font without distributing font files."""
    candidates = (
        "DejaVuSans.ttf",
        "/System/Library/Fonts/HelveticaNeue.ttc",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    )
    for candidate in candidates:
        try:
            return ImageFont.truetype(candidate, size=size)
        except OSError:
            continue
    raise RuntimeError("Install DejaVu Sans to render README illustrations.")


def text(
    draw: ImageDraw.ImageDraw, xy: tuple[int, int], value: str, style: TextStyle
) -> None:
    """Draw one label and fail if it would exceed its allocated width."""
    face = font(style.size)
    if draw.textlength(value, font=face) > style.max_width:
        raise ValueError(f"Label exceeds allocated width: {value}")
    draw.text(xy, value, font=face, fill=style.color)


def frame(progress: float) -> Image.Image:
    """Illustrate generation, aggregation and a compact result."""
    image = Image.new("RGB", SIZE, BACKGROUND)
    draw = ImageDraw.Draw(image)
    cards = (
        ("Generate sales", "seed=42 / rows=3"),
        ("Calculate totals", "count / units / revenue"),
        ("Return report", "3 sales / 8 units / 16,998 cents"),
    )
    active = min(int(progress * 3), 2)
    for index, (title, detail) in enumerate(cards):
        top = 24 + index * 184
        draw.rounded_rectangle(
            (24, top, 696, top + 144),
            radius=16,
            fill="#111e30",
            outline=ACCENT if index == active else "#2a3b53",
            width=2,
        )
        draw.rounded_rectangle(
            (48, top + 44, 104, top + 100), radius=16, fill="#123331"
        )
        text(
            draw,
            (66, top + 48),
            str(index + 1),
            TextStyle(36, ACCENT if index <= active else MUTED, 40),
        )
        text(draw, (128, top + 25), title, TextStyle(38))
        text(draw, (128, top + 83), detail, TextStyle(32, MUTED))
    for top, bottom in ((176, 200), (360, 384)):
        draw.line((360, top, 360, bottom - 7), fill="#425873", width=2)
        draw.polygon(
            ((355, bottom - 9), (360, bottom), (365, bottom - 9)), fill="#425873"
        )
    if active < 2:
        fraction = progress * 3 - active
        y = 176 + active * 184 + round(fraction * 17)
        draw.ellipse((355, y - 5, 365, y + 5), fill=ACCENT)
    return image


def main() -> None:
    """Write the animated and static assets with the same labels and layout."""
    ASSETS.mkdir(parents=True, exist_ok=True)
    frames = [frame(index / 53) for index in range(54)]
    frames[-1].save(ASSETS / "labflow-flow.png", optimize=True)
    palette = frames[-1].quantize(colors=128, dither=Image.Dither.NONE)
    indexed = [
        image.quantize(palette=palette, dither=Image.Dither.NONE) for image in frames
    ]
    indexed[0].save(
        ASSETS / "labflow-flow.gif",
        save_all=True,
        append_images=indexed[1:],
        duration=[80] * 53 + [1200],
        loop=0,
        optimize=True,
        disposal=1,
        comment=b"Illustration of the local Python calculation; not a runtime trace.",
    )
    print("Rendered docs/assets/labflow-flow.gif and labflow-flow.png")


if __name__ == "__main__":
    main()
