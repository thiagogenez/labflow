"""Bounded order quotations using one store's trusted price list."""

from dataclasses import dataclass
from typing import Self

from labflow.catalog import validate_identifier
from labflow.currency import MAX_AMOUNT_CENTS, Currency
from labflow.price_list import PriceList
from labflow.store import validate_store_configuration
from labflow.validation import validate_integer_range

MAX_ORDER_LINES = 20
MAX_QUANTITY = 100


def _validate_item(product_id: str, quantity: int) -> None:
    """Validate the fields shared by requested and quoted items.

    Args:
        product_id: Nonempty product identifier without surrounding whitespace.
        quantity: Integer unit count from 1 through MAX_QUANTITY; booleans are
            rejected even though Python treats them as integers.

    Raises:
        TypeError: An argument has an unsupported type.
        ValueError: An identifier is invalid or quantity is outside its bounds.
    """
    validate_identifier(product_id, "product_id")
    validate_integer_range(quantity, "quantity", minimum=1, maximum=MAX_QUANTITY)


@dataclass(frozen=True, slots=True)
class OrderItem:
    """A buyer's requested product and quantity, without a trusted price.

    Args:
        product_id: Nonempty product identifier without surrounding whitespace,
            such as "camiseta". Existence is checked during catalog lookup.
        quantity: Integer number of units, from 1 through MAX_QUANTITY (100).
            Booleans, floats and strings are not accepted.

    Raises:
        TypeError: A field has an unsupported type.
        ValueError: The identifier is invalid or quantity exceeds its bounds.
    """

    product_id: str
    quantity: int

    def __post_init__(self) -> None:
        """Validate buyer-supplied fields after dataclass initialization."""
        _validate_item(self.product_id, self.quantity)


@dataclass(frozen=True, slots=True)
class QuotedItem:
    """An item carrying a copied catalog price in integer cents.

    Args:
        product_id: Nonempty product identifier without surrounding whitespace.
        quantity: Integer unit count from 1 through MAX_QUANTITY (100), excluding
            booleans.
        unit_price_cents: Store price for one unit, from 0 through
            MAX_AMOUNT_CENTS (2**63 - 1). For example, 1990 means 19.90 in the
            store's two-decimal currency. Booleans and floats are rejected.

    Raises:
        TypeError: A field has an unsupported type.
        ValueError: An identifier, quantity or unit price is outside its bounds.
    """

    product_id: str
    quantity: int
    unit_price_cents: int

    def __post_init__(self) -> None:
        """Validate item fields and the copied catalog price."""
        _validate_item(self.product_id, self.quantity)
        validate_integer_range(
            self.unit_price_cents,
            "unit_price_cents",
            minimum=0,
            maximum=MAX_AMOUNT_CENTS,
            range_message="unit_price_cents is outside the supported amount range",
        )

    @property
    def total_cents(self) -> int:
        """Return quantity times unit price as an integer amount in cents."""
        return self.quantity * self.unit_price_cents


@dataclass(frozen=True, slots=True)
class OrderQuote:
    """An immutable price snapshot, without order acceptance or stock reservation.

    Args:
        store_id: Nonempty store identifier without surrounding whitespace,
            copied from the price list. It is not proof of store authorization.
        currency: Currency enum member of all quoted amounts, copied from the
            store's price list. EUR and BRL use integer cents; raw strings are rejected.
        catalog_version: Identifier of the shared product metadata snapshot.
        price_list_version: Identifier of the store price snapshot used to quote.
            Both version identifiers must be nonempty without surrounding spaces.
        items: Tuple of 1 through MAX_ORDER_LINES (20) QuotedItem instances with
            distinct product identifiers. Their combined amount must not exceed
            MAX_AMOUNT_CENTS (2**63 - 1).

    Raises:
        TypeError: Store, currency, version, container or item types are invalid.
        ValueError: Store or version is invalid, line count is outside its
            bounds, products repeat or the combined amount exceeds its bounds.
    """

    store_id: str
    currency: Currency
    catalog_version: str
    price_list_version: str
    items: tuple[QuotedItem, ...]

    def __post_init__(self) -> None:
        """Validate the snapshot metadata, basket and combined amount."""
        validate_store_configuration(self.store_id, self.currency)
        validate_identifier(self.catalog_version, "catalog_version")
        validate_identifier(self.price_list_version, "price_list_version")
        if not isinstance(self.items, tuple):
            raise TypeError("quoted items must be a tuple")
        if not 1 <= len(self.items) <= MAX_ORDER_LINES:
            raise ValueError(f"orders must contain 1 to {MAX_ORDER_LINES} lines")
        if any(not isinstance(item, QuotedItem) for item in self.items):
            raise TypeError("quoted items must contain QuotedItem instances")
        if len({item.product_id for item in self.items}) != len(self.items):
            raise ValueError("duplicate product lines are not allowed")
        if self.total_cents > MAX_AMOUNT_CENTS:
            raise ValueError("order total exceeds the supported amount range")

    @property
    def total_cents(self) -> int:
        """Return the sum of quoted line amounts as integer cents."""
        return sum(item.total_cents for item in self.items)

    @classmethod
    def from_price_list(
        cls,
        *,
        items: tuple[OrderItem, ...],
        price_list: PriceList,
    ) -> Self:
        """Quote requested items from one store's immutable price list.

        Args:
            items: Tuple of 1 through MAX_ORDER_LINES (20) distinct OrderItem
                instances with quantities from 1 through 100.
            price_list: Server-selected PriceList binding a Store, shared Catalog
                and that store's offered prices. It must offer every requested
                SKU. Selection and authorization belong to the caller.

        Returns:
            An immutable quote preserving store, currency, product-catalog
            version, price-list version and copied integer-cent prices.

        Raises:
            TypeError: Items or price_list have unsupported types.
            ValueError: Basket bounds are violated, products repeat, a requested
                SKU is not offered or the combined amount exceeds its bounds.

        This calculation performs no inventory, authorization or persistence
        operations and never converts currencies.

        Example:
            >>> from labflow.store import Store
            >>> from labflow.catalog import Catalog, Product
            >>> store = Store("store-berlin", Currency.EUR)
            >>> catalog = Catalog("products-v1", {"shirt": Product("shirt", "Shirt")})
            >>> prices = PriceList(store, "prices-v1", {"shirt": 1990}, catalog)
            >>> quote = OrderQuote.from_price_list(
            ...     items=(OrderItem("shirt", 2),), price_list=prices,
            ... )
            >>> quote.total_cents
            3980
        """
        if not isinstance(items, tuple):
            raise TypeError("items must be a tuple")
        if not 1 <= len(items) <= MAX_ORDER_LINES:
            raise ValueError(f"orders must contain 1 to {MAX_ORDER_LINES} lines")
        if any(not isinstance(item, OrderItem) for item in items):
            raise TypeError("items must contain OrderItem instances")
        if len({item.product_id for item in items}) != len(items):
            raise ValueError("duplicate product lines are not allowed")
        if not isinstance(price_list, PriceList):
            raise TypeError("price_list must be a PriceList instance")

        quoted = []
        for item in items:
            try:
                price = price_list.prices[item.product_id]
            except KeyError as error:
                raise ValueError(
                    f"product not offered by store: {item.product_id}"
                ) from error
            quoted.append(QuotedItem(item.product_id, item.quantity, price))
        return cls(
            store_id=price_list.store_id,
            currency=price_list.currency,
            catalog_version=price_list.catalog.version,
            price_list_version=price_list.version,
            items=tuple(quoted),
        )
