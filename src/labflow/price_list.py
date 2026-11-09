"""Immutable, server-controlled price snapshots bound to one store."""

from collections.abc import Mapping
from dataclasses import dataclass
from types import MappingProxyType

from labflow.catalog import Catalog, validate_identifier
from labflow.currency import MAX_AMOUNT_CENTS, Currency
from labflow.store import Store


@dataclass(frozen=True, slots=True)
class PriceList:
    """Keep one store's prices bound to a shared product catalog snapshot.

    Args:
        store: Server-controlled Store configuration supplying the identifier
            and fixed currency. No independent store_id or currency arguments
            are accepted. This configuration is not proof of authorization.
        version: Nonempty snapshot identifier without surrounding whitespace,
            such as "prices-v1". Assign a new version when publishing new prices.
        catalog: Immutable, shared Catalog containing every priced product.
            The list may offer only a subset of its products.
        prices: Server-controlled mapping from nonempty product identifiers
            without surrounding whitespace to integer unit prices in cents.
            Amounts use the store's currency, with no conversion. Prices range
            from 0 through MAX_AMOUNT_CENTS (2**63 - 1); booleans
            and floats are rejected. Empty price lists are allowed. The mapping
            must be stable during construction and is copied before exposure
            through a read-only view, so later source edits cannot alter it.

    Raises:
        TypeError: Metadata, the mapping, a product identifier or a price has an
            unsupported type.
        ValueError: An identifier is invalid, a priced SKU is absent from the
            catalog or a price is outside the supported amount range.

    A version is descriptive metadata, not a content hash. Persistence will need
    to enforce the association between a store, version and its prices.
    """

    store: Store
    version: str
    prices: Mapping[str, int]
    catalog: Catalog

    def __post_init__(self) -> None:
        """Validate metadata and prices, then freeze a copy of the mapping."""
        if not isinstance(self.store, Store):
            raise TypeError("store must be a Store instance")
        if not isinstance(self.catalog, Catalog):
            raise TypeError("catalog must be a Catalog instance")
        validate_identifier(self.version, "version")
        if not isinstance(self.prices, Mapping):
            raise TypeError("prices must be a mapping")

        prices = dict(self.prices)
        for product_id, price in prices.items():
            validate_identifier(product_id, "product_id")
            if product_id not in self.catalog.products:
                raise ValueError(f"product is absent from catalog: {product_id}")
            if isinstance(price, bool) or not isinstance(price, int):
                raise TypeError("unit_price_cents must be an integer")
            if not 0 <= price <= MAX_AMOUNT_CENTS:
                raise ValueError(
                    "unit_price_cents is outside the supported amount range"
                )
        object.__setattr__(self, "prices", MappingProxyType(prices))

    @property
    def store_id(self) -> str:
        """Return the identifier from the price list's store configuration."""
        return self.store.store_id

    @property
    def currency(self) -> Currency:
        """Return the store's fixed currency for every offered price."""
        return self.store.currency
