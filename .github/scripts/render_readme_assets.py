"""Render the illustrative local-calculation flow used in the README."""

from dataclasses import dataclass
from functools import cache
from math import pi, sin
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

from labflow.reporting import summarize_sales
from labflow.synthetic import GENERATOR_VERSION, generate_sales

ROOT = Path(__file__).resolve().parents[2]
ASSETS = ROOT / "docs" / "assets"
SIZE = (720, 600)
BACKGROUND = "#0b1821"
SURFACE = "#132631"
TEXT = "#eef5f5"
MUTED = "#a6bbc5"
ACCENT = "#65dec2"
LINE = "#304853"
SALES = generate_sales(seed=42, rows=3)
REPORT = summarize_sales(SALES)


@dataclass(frozen=True)
class TextStyle:
    """Font size, color and width constraint for a figure label."""

    size: int
    color: str = TEXT
    max_width: int = 656


@cache
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
    draw.text(xy, value, font=face, fill=style.color, anchor="lt")


def heading(draw: ImageDraw.ImageDraw, progress: float) -> None:
    """Draw the title and a quiet wave motif without a hosted asset."""
    for offset, color in ((0, "#173541"), (14, "#204b52"), (28, "#285b5c")):
        points = [
            (x, 145 + offset + round(10 * sin(x / 90 + progress * 2 * pi)))
            for x in range(430, 721, 3)
        ]
        draw.line(points, fill=color, width=2)
    text(draw, (32, 28), "LABFLOW  /  THE REPORTING CORE", TextStyle(15, ACCENT))
    text(draw, (32, 65), "Same seed. Same sales.", TextStyle(38))
    text(
        draw, (32, 116), "Follow the data from input to summary.", TextStyle(22, MUTED)
    )
    draw.rounded_rectangle((32, 158, 385, 194), radius=8, fill=SURFACE)
    text(
        draw,
        (46, 167),
        f"seed=42   rows=3   {GENERATOR_VERSION}",
        TextStyle(20, ACCENT, 325),
    )


def sales_sheet(draw: ImageDraw.ImageDraw, progress: float) -> None:
    """Illustrate the three actual sample records as an itemized receipt."""
    text(draw, (32, 227), "01 / GENERATE", TextStyle(16, MUTED, 292))
    draw.rounded_rectangle((32, 260, 324, 463), radius=10, fill=SURFACE, outline=LINE)
    for x in range(33, 317, 16):
        draw.polygon(((x, 464), (x + 8, 454), (x + 16, 464)), fill=BACKGROUND)
    for x, label in ((48, "Qty"), (108, "Price"), (222, "Total")):
        text(draw, (x, 279), label, TextStyle(18, MUTED, 85))
    draw.line((48, 309, 308, 309), fill=LINE, width=1)
    for index, sale in enumerate(SALES):
        top = 322 + index * 42
        selected = 0.12 + index * 0.18 <= progress < 0.30 + index * 0.18
        if selected:
            draw.rounded_rectangle(
                (41, top - 5, 315, top + 28), radius=5, fill="#234638"
            )
        values = (
            str(sale.quantity),
            f"{sale.unit_price_cents:,}",
            f"{sale.total_cents:,}",
        )
        for x, value in zip((48, 108, 222), values):
            text(draw, (x, top), value, TextStyle(22, ACCENT if selected else TEXT, 86))
    text(draw, (48, 436), "Prices and totals in cents", TextStyle(15, MUTED, 260))


def processor(draw: ImageDraw.ImageDraw, progress: float) -> None:
    """Move one illustrated record at a time through the aggregation chip."""
    text(draw, (346, 227), "02 / SUM", TextStyle(16, MUTED, 110))
    working = 0.12 <= progress < 0.72
    color = ACCENT if working else LINE
    for position in range(338, 381, 14):
        draw.line((357, position, 369, position), fill=color, width=2)
        draw.line((427, position, 439, position), fill=color, width=2)
    for position in range(377, 420, 14):
        draw.line((position, 318, position, 330), fill=color, width=2)
        draw.line((position, 388, position, 400), fill=color, width=2)
    draw.rounded_rectangle(
        (369, 330, 427, 388), radius=8, fill=SURFACE, outline=color, width=2
    )
    text(draw, (382, 339), "Σ", TextStyle(34, ACCENT, 38))
    for start, end in ((325, 356), (440, 470)):
        draw.line((start, 359, end, 359), fill=LINE, width=2)
        draw.polygon(((end - 5, 354), (end, 359), (end - 5, 364)), fill=MUTED)
    if working:
        position = round(325 + ((progress - 0.12) / 0.18 % 1) * 145)
        if position < 357 or position > 439:
            draw.ellipse((position - 4, 355, position + 4, 363), fill=ACCENT)


def report_sheet(draw: ImageDraw.ImageDraw, progress: float) -> None:
    """Reveal the compact result after the illustrated calculation finishes."""
    text(draw, (478, 227), "03 / REPORT", TextStyle(16, MUTED, 210))
    ready = progress >= 0.72
    draw.polygon(
        ((478, 260), (660, 260), (688, 288), (688, 463), (478, 463)),
        fill=SURFACE,
        outline=ACCENT if ready else LINE,
    )
    draw.line((660, 260, 660, 288, 688, 288), fill=ACCENT if ready else LINE, width=1)
    for top, label, key in (
        (284, "sales", "sales_count"),
        (338, "units", "units_sold"),
        (392, "cents", "revenue_cents"),
    ):
        value = f"{REPORT[key]:,}" if ready else "—"
        text(draw, (496, top), value, TextStyle(30, ACCENT if ready else MUTED, 170))
        text(draw, (496, top + 32), label, TextStyle(15, MUTED, 170))


def timeline(draw: ImageDraw.ImageDraw, progress: float) -> None:
    """Show the current phase; timing is illustrative, never a benchmark."""
    if progress < 0.12:
        label = "Generate three reproducible sale records."
    elif progress < 0.72:
        label = "Count sales. Add units. Sum quantity × price."
    else:
        label = "Return one compact report."
    text(draw, (32, 500), label, TextStyle(23))
    draw.rounded_rectangle((32, 541, 688, 545), radius=2, fill=LINE)
    draw.rounded_rectangle(
        (32, 541, 33 + round(progress * 655), 545), radius=2, fill=ACCENT
    )
    text(draw, (32, 565), "LOCAL PYTHON  ·  ILLUSTRATED FLOW", TextStyle(14, MUTED))
    text(draw, (513, 565), "No services required", TextStyle(16, MUTED, 175))


def frame(progress: float) -> Image.Image:
    """Compose the receipt, computation and result at one animation position."""
    image = Image.new("RGB", SIZE, BACKGROUND)
    draw = ImageDraw.Draw(image)
    draw.rounded_rectangle((0, 0, 719, 599), radius=16, outline=LINE)
    heading(draw, progress)
    sales_sheet(draw, progress)
    processor(draw, progress)
    report_sheet(draw, progress)
    timeline(draw, progress)
    return image


def main() -> None:
    """Write the animated and static assets with the same labels and layout."""
    ASSETS.mkdir(parents=True, exist_ok=True)
    frames = [frame(index / 71) for index in range(72)]
    frames[-1].save(ASSETS / "labflow-flow.png", optimize=True)
    # Include every phase so highlights and moving elements share stable colors.
    samples = Image.new("RGB", (SIZE[0], SIZE[1] * 3))
    for index, source in enumerate((frames[0], frames[30], frames[-1])):
        samples.paste(source, (0, index * SIZE[1]))
    palette = samples.quantize(colors=128, dither=Image.Dither.NONE)
    indexed = [
        image.quantize(palette=palette, dither=Image.Dither.NONE) for image in frames
    ]
    indexed[0].save(
        ASSETS / "labflow-flow.gif",
        save_all=True,
        append_images=indexed[1:],
        duration=[90] * 71 + [1800],
        loop=0,
        optimize=True,
        disposal=1,
        comment=b"Illustration of the local Python calculation; not a runtime trace.",
    )
    print("Rendered docs/assets/labflow-flow.gif and labflow-flow.png")


if __name__ == "__main__":
    main()
