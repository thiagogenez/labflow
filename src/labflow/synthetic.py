"""Versioned synthetic sales with bounded size and no shared random state."""

from dataclasses import dataclass
from random import Random

GENERATOR_VERSION = "sales-v1"
MAX_ROWS = 10_000
MAX_SEED = 2**32 - 1


@dataclass(frozen=True, slots=True)
class Sale:
    """One synthetic sale; row_id is unique only within its dataset."""

    row_id: int
    product_id: int
    quantity: int
    unit_price_cents: int

    @property
    def total_cents(self) -> int:
        """Total price in cents."""
        return self.quantity * self.unit_price_cents


def generate_sales(*, seed: int, rows: int) -> tuple[Sale, ...]:
    """Generate sales reproducibly for this generator and Python version.

    Prices are integer cents. Validate bounds before allocating the dataset.
    Callers must retain the seed, row count, generator version, and runtime
    version when recording an experiment.
    """
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise TypeError("seed must be an integer")
    if not 0 <= seed <= MAX_SEED:
        raise ValueError(f"seed must be between 0 and {MAX_SEED}")
    if isinstance(rows, bool) or not isinstance(rows, int):
        raise TypeError("rows must be an integer")
    if not 1 <= rows <= MAX_ROWS:
        raise ValueError(f"rows must be between 1 and {MAX_ROWS}")

    random = Random(seed)
    return tuple(
        Sale(
            row_id=row_id,
            product_id=random.randint(1, 10),
            quantity=random.randint(1, 5),
            unit_price_cents=random.randint(100, 10_000),
        )
        for row_id in range(1, rows + 1)
    )
