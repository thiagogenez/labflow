"""Render the planned Kafka/worker flow as compact README illustrations."""

from dataclasses import dataclass
from functools import cache
from itertools import pairwise
from math import hypot
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
ASSETS = ROOT / "docs" / "assets"
SIZE = (840, 320)
BACKGROUND = "#0b1821"
SURFACE = "#132631"
TEXT = "#eef5f5"
MUTED = "#a6bbc5"
ACCENT = "#65dec2"
LINE = "#304853"


@dataclass(frozen=True)
class TextStyle:
    """Font size, color and width constraint for a figure label."""

    size: int
    color: str = TEXT
    max_width: int = 792


@dataclass(frozen=True)
class WorkerLane:
    """One distinct example order, its assigned worker and its result color."""

    number: int
    center_y: int
    color: str


WORKERS = (
    WorkerLane(1, 126, ACCENT),
    WorkerLane(2, 182, "#83baf4"),
    WorkerLane(3, 238, "#f0c477"),
)


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


def connection(draw: ImageDraw.ImageDraw, path: tuple[tuple[int, int], ...]) -> None:
    """Draw a route ending with a right-pointing arrow."""
    draw.line(path, fill=LINE, width=2)
    x, y = path[-1]
    draw.polygon(((x - 6, y - 4), (x, y), (x - 6, y + 4)), fill=MUTED)


def point_on_path(
    path: tuple[tuple[int, int], ...], fraction: float
) -> tuple[int, int]:
    """Interpolate along a routed connection at a constant visual speed."""
    lengths = [hypot(b[0] - a[0], b[1] - a[1]) for a, b in pairwise(path)]
    distance = max(0.0, min(fraction, 1.0)) * sum(lengths)
    for (start, end), length in zip(pairwise(path), lengths):
        if length and distance <= length:
            ratio = distance / length
            return (
                round(start[0] + ratio * (end[0] - start[0])),
                round(start[1] + ratio * (end[1] - start[1])),
            )
        distance -= length
    return path[-1]


def packet(
    draw: ImageDraw.ImageDraw,
    path: tuple[tuple[int, int], ...],
    fraction: float,
    lane: WorkerLane,
    *,
    compact: bool = False,
) -> None:
    """Keep each order's color through computation and compact result delivery."""
    x, y = point_on_path(path, fraction)
    radius = 5 if compact else 8
    draw.rounded_rectangle(
        (x - radius, y - radius, x + radius, y + radius), radius=3, fill=lane.color
    )


def infrastructure(draw: ImageDraw.ImageDraw) -> None:
    """Show topics, the result consumer and the authoritative database."""
    text(draw, (24, 22), "Planned order processing", TextStyle(28))
    text(draw, (646, 30), "KUBERNETES", TextStyle(18, MUTED, 170))
    text(draw, (24, 91), "Kafka topic", TextStyle(18, MUTED, 104))
    draw.rounded_rectangle((24, 128, 128, 249), radius=8, fill=SURFACE, outline=LINE)
    text(draw, (40, 140), "requests", TextStyle(18, TEXT, 78))
    for lane in WORKERS:
        top = 174 + (lane.number - 1) * 24
        draw.rounded_rectangle((36, top - 7, 116, top + 12), radius=4, outline=LINE)
        text(draw, (44, top - 5), f"p{lane.number - 1}", TextStyle(15, lane.color, 60))

    text(draw, (224, 65), "Worker pods", TextStyle(22, TEXT, 154))
    text(draw, (224, 92), "check order", TextStyle(16, MUTED, 154))
    text(draw, (462, 117), "Kafka topic", TextStyle(18, MUTED, 104))
    draw.rounded_rectangle((462, 153, 566, 211), radius=8, fill=SURFACE, outline=LINE)
    text(draw, (472, 170), "results", TextStyle(23, TEXT, 84))

    draw.rounded_rectangle((616, 153, 706, 211), radius=8, fill=SURFACE, outline=LINE)
    text(draw, (631, 162), "Result", TextStyle(20, TEXT, 66))
    text(draw, (626, 188), "consumer", TextStyle(16, MUTED, 74))
    connection(draw, ((566, 182), (616, 182)))
    connection(draw, ((706, 182), (754, 182)))

    text(draw, (718, 117), "PostgreSQL", TextStyle(19, MUTED, 106))
    draw.rectangle((754, 162, 818, 204), fill=SURFACE, outline=LINE)
    draw.ellipse((754, 194, 818, 214), fill=SURFACE, outline=LINE)
    draw.ellipse((754, 152, 818, 172), fill=SURFACE, outline=LINE)
    text(draw, (724, 230), "order + stock", TextStyle(15, MUTED, 100))
    text(draw, (616, 230), "finalize", TextStyle(16, MUTED, 90))
    text(
        draw,
        (24, 264),
        "API reserves stock before publishing accepted orders.",
        TextStyle(17, MUTED),
    )
    text(
        draw,
        (24, 289),
        "One Kafka cluster · requests + results · 3 example orders",
        TextStyle(18, MUTED),
    )


def worker(draw: ImageDraw.ImageDraw, lane: WorkerLane, progress: float) -> None:
    """Illustrate a distinct order entering, computing and leaving one worker."""
    phase = progress * 1.2 - (lane.number - 1) * 0.12
    source_y = 174 + (lane.number - 1) * 24
    incoming = (
        (128, source_y),
        (174, source_y),
        (174, lane.center_y),
        (224, lane.center_y),
    )
    outgoing = ((378, lane.center_y), (418, lane.center_y), (418, 182), (462, 182))
    connection(draw, incoming)
    connection(draw, outgoing)
    active = 0.18 <= phase < 0.54
    draw.rounded_rectangle(
        (224, lane.center_y - 17, 378, lane.center_y + 17),
        radius=7,
        fill="#203c45" if active else SURFACE,
        outline=lane.color,
    )
    draw.ellipse((235, lane.center_y - 4, 243, lane.center_y + 4), fill=lane.color)
    text(
        draw,
        (254, lane.center_y - 10),
        f"Worker {lane.number}",
        TextStyle(21, TEXT, 116),
    )
    if active:
        end = 226 + round((phase - 0.18) / 0.36 * 149)
        draw.line(
            (226, lane.center_y + 16, end, lane.center_y + 16), fill=lane.color, width=2
        )
    if 0 <= phase < 0.18:
        packet(draw, incoming, phase / 0.18, lane)
    elif 0.54 <= phase < 0.74:
        packet(draw, outgoing, (phase - 0.54) / 0.2, lane, compact=True)
    elif 0.74 <= phase < 0.94:
        # Pass through the result consumer; workers never write to the database.
        packet(draw, ((566, 182), (782, 182)), (phase - 0.74) / 0.2, lane, compact=True)
    elif phase >= 0.94:
        x = 765 + (lane.number - 1) * 17
        draw.ellipse((x - 4, 182, x + 4, 190), fill=lane.color)


def frame(progress: float) -> Image.Image:
    """Compose three concurrent example orders without implying live execution."""
    image = Image.new("RGB", SIZE, BACKGROUND)
    draw = ImageDraw.Draw(image)
    draw.rounded_rectangle((0, 0, SIZE[0] - 1, SIZE[1] - 1), radius=12, outline=LINE)
    infrastructure(draw)
    for lane in WORKERS:
        worker(draw, lane, progress)
    return image


def main() -> None:
    """Write the animated and static assets with the same labels and layout."""
    ASSETS.mkdir(parents=True, exist_ok=True)
    frames = [frame(index / 71) for index in range(72)]
    frames[-1].save(ASSETS / "labflow-flow.png", optimize=True)
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
        duration=[90] * 71 + [1200],
        loop=0,
        optimize=True,
        disposal=1,
        comment=b"Illustrative accepted orders; not a runtime trace.",
    )
    print("Rendered docs/assets/labflow-flow.gif and labflow-flow.png")


if __name__ == "__main__":
    main()
