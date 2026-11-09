"""Immutable inventory read snapshots; stock writes belong to persistence."""

from dataclasses import dataclass

from labflow.catalog import Product
from labflow.store import Store
from labflow.validation import validate_integer_range

MAX_STOCK_UNITS = 2**63 - 1


@dataclass(frozen=True, slots=True)
class InventorySnapshot:
    """An immutable stock snapshot for one store and one product/SKU.

    Args:
        store: Store configuration identifying the unit holding this stock.
            The reference does not authorize access to that store.
        product: Product metadata identifying the SKU. Price-list membership
            and whether the store offers this product are separate concerns.
        on_hand: Integer physical stock count, from 0 through MAX_STOCK_UNITS
            (2**63 - 1). This includes units currently reserved.
        reserved: Integer units already reserved, from 0 through on_hand.
            Defaults to 0. Booleans, floats and strings are rejected for counts.

    Raises:
        TypeError: Store/product references or stock counts have invalid types.
        ValueError: Counts exceed their bounds or reserved exceeds on_hand.

    Available stock is derived, never supplied separately. This object performs
    no persistence, stock updates, order tracking or concurrency control. A
    snapshot can become stale; availability is not authorization to reserve.
    Planned stock writes use conditional PostgreSQL updates in transactions.
    """

    store: Store
    product: Product
    on_hand: int
    reserved: int = 0

    def __post_init__(self) -> None:
        """Validate references and require 0 <= reserved <= on_hand."""
        if not isinstance(self.store, Store):
            raise TypeError("store must be a Store instance")
        if not isinstance(self.product, Product):
            raise TypeError("product must be a Product instance")
        validate_integer_range(
            self.on_hand, "on_hand", minimum=0, maximum=MAX_STOCK_UNITS
        )
        validate_integer_range(
            self.reserved, "reserved", minimum=0, maximum=MAX_STOCK_UNITS
        )
        if self.reserved > self.on_hand:
            raise ValueError("reserved must not exceed on_hand")

    @property
    def store_id(self) -> str:
        """Return the store part of the future persisted balance key."""
        return self.store.store_id

    @property
    def product_id(self) -> str:
        """Return the SKU part of the future persisted balance key."""
        return self.product.product_id

    @property
    def available(self) -> int:
        """Return on-hand units minus reserved units."""
        return self.on_hand - self.reserved
