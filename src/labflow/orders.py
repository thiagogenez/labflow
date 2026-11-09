"""Immutable order quotations and processing lifecycle rules."""

from dataclasses import dataclass, replace
from datetime import datetime
from enum import StrEnum
from typing import Self
from uuid import UUID

from labflow.catalog import validate_identifier
from labflow.currency import MAX_AMOUNT_CENTS, Currency
from labflow.price_list import PriceList
from labflow.store import validate_store_configuration
from labflow.validation import normalize_utc_datetime, validate_integer_range

MAX_ORDER_LINES = 20
MAX_QUANTITY = 100


class OrderStatus(StrEnum):
    """States of the current asynchronous order-processing lifecycle.

    Pending includes queued work, active processing and bounded retries. Jobs
    and attempts track those execution details. Confirmation implies neither
    payment nor shipment; payment, fulfillment and cancellation require separate
    lifecycle rules when introduced.
    """

    PENDING = "pending"
    CONFIRMED = "confirmed"
    REJECTED = "rejected"
    PROCESSING_FAILED = "processing_failed"


class OrderFailureReason(StrEnum):
    """Safe reasons for business rejection or definitive processing failure.

    Pending and confirmed orders have no failure reason. Retry execution details
    belong in separate attempt records.
    """

    BUSINESS_RULE_VIOLATION = "business_rule_violation"
    RETRY_EXHAUSTED = "retry_exhausted"
    RESERVATION_EXPIRED = "reservation_expired"


_ALLOWED_FAILURE_REASONS = {
    OrderStatus.PENDING: (None,),
    OrderStatus.CONFIRMED: (None,),
    OrderStatus.REJECTED: (OrderFailureReason.BUSINESS_RULE_VIOLATION,),
    OrderStatus.PROCESSING_FAILED: (
        OrderFailureReason.RETRY_EXHAUSTED,
        OrderFailureReason.RESERVATION_EXPIRED,
    ),
}


class InvalidOrderTransitionError(ValueError):
    """A processing transition would reopen or replace a final decision."""


def _validate_resolution(
    status: OrderStatus, reason: OrderFailureReason | None
) -> None:
    """Match each processing state to its permitted safe reason codes."""
    if not isinstance(status, OrderStatus):
        raise TypeError("status must be an OrderStatus")
    if reason is not None and not isinstance(reason, OrderFailureReason):
        raise TypeError("reason must be an OrderFailureReason or None")
    if reason not in _ALLOWED_FAILURE_REASONS[status]:
        raise ValueError(f"reason is incompatible with status {status.value}")


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


def _validate_items(
    items: tuple[OrderItem, ...] | tuple[QuotedItem, ...],
    item_type: type[OrderItem | QuotedItem],
    *,
    name: str,
) -> None:
    """Validate the basket rules shared by requested and quoted items.

    Args:
        items: Tuple of 1 through MAX_ORDER_LINES items with distinct SKUs.
        item_type: Required domain type, OrderItem or QuotedItem.
        name: Basket name used in errors, such as "items" or "quoted items".

    Raises:
        TypeError: The container is not a tuple or contains an invalid item type.
        ValueError: The basket is empty, exceeds its line budget or repeats a SKU.
    """
    if not isinstance(items, tuple):
        raise TypeError(f"{name} must be a tuple")
    if not 1 <= len(items) <= MAX_ORDER_LINES:
        raise ValueError(f"orders must contain 1 to {MAX_ORDER_LINES} lines")
    if any(not isinstance(item, item_type) for item in items):
        raise TypeError(f"{name} must contain {item_type.__name__} instances")
    if len({item.product_id for item in items}) != len(items):
        raise ValueError("duplicate product lines are not allowed")


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
        _validate_items(self.items, QuotedItem, name="quoted items")
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
        _validate_items(items, OrderItem, name="items")
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


@dataclass(frozen=True, slots=True, kw_only=True)
class Order:
    """An accepted order snapshot with guarded processing transitions.

    Args:
        order_id: Server-assigned UUID, suitable for a future PostgreSQL UUID key.
        buyer_id: Nonempty trusted buyer identifier without surrounding spaces.
            It identifies ownership but does not perform authorization.
        quote: Frozen accepted items, prices, currency, store and price versions.
        accepted_at: Server-assigned, offset-aware acceptance time; normalized
            to UTC. Persistence must supply the authoritative database time.
        status: Processing state, defaulting to PENDING. Raw strings are rejected.
        resolved_at: Offset-aware final-decision time, normalized to UTC. Required
            for terminal states, absent for PENDING, never before accepted_at.
        reason: Safe enum code, required for rejection or processing failure.
            PENDING and CONFIRMED require None.

    Raises:
        TypeError: A field has an unsupported type.
        ValueError: An identifier, timestamp or state/reason combination is invalid.

    Constructing this object does not accept an order durably or reserve stock.
    Terminal construction supports future database rehydration. Resolution is a
    pure proposal: database state/attempt/deadline guards must authorize its commit.
    This object provides no locking, HTTP idempotency or event deduplication.
    """

    order_id: UUID
    buyer_id: str
    quote: OrderQuote
    accepted_at: datetime
    status: OrderStatus = OrderStatus.PENDING
    resolved_at: datetime | None = None
    reason: OrderFailureReason | None = None

    def __post_init__(self) -> None:
        """Validate identity, frozen quote and coherent lifecycle metadata."""
        if not isinstance(self.order_id, UUID):
            raise TypeError("order_id must be a UUID")
        validate_identifier(self.buyer_id, "buyer_id")
        if not isinstance(self.quote, OrderQuote):
            raise TypeError("quote must be an OrderQuote")
        _validate_resolution(self.status, self.reason)
        accepted_at = normalize_utc_datetime(self.accepted_at, "accepted_at")
        object.__setattr__(self, "accepted_at", accepted_at)
        if self.status is OrderStatus.PENDING:
            if self.resolved_at is not None:
                raise ValueError("pending orders cannot have resolved_at")
        else:
            resolved_at = normalize_utc_datetime(self.resolved_at, "resolved_at")
            if resolved_at < accepted_at:
                raise ValueError("resolved_at must not precede accepted_at")
            object.__setattr__(self, "resolved_at", resolved_at)

    def resolve(
        self,
        status: OrderStatus,
        *,
        at: datetime,
        reason: OrderFailureReason | None = None,
    ) -> Self:
        """Propose a terminal processing decision for a pending order.

        Args:
            status: CONFIRMED, REJECTED or PROCESSING_FAILED enum member.
            at: Trusted offset-aware decision time, not earlier than acceptance.
                Future persistence must obtain authoritative database time.
            reason: BUSINESS_RULE_VIOLATION for rejection, RETRY_EXHAUSTED or
                RESERVATION_EXPIRED for technical failure, None for confirmation.

        Returns:
            A new Order retaining identity, buyer, quote and acceptance time.
            The original is unchanged. No inventory or database writes occur.

        Raises:
            InvalidOrderTransitionError: The order is terminal or the target is
                PENDING. Repeated terminal calls also fail; duplicate handling
                belongs to the future transactional persistence operation.
            TypeError: Target, timestamp or reason has an unsupported type.
            ValueError: Reason is incompatible or the decision time is invalid.

        Technical retries do not call this method: the order remains pending.
        Confirmation keeps stock committed until a later fulfillment operation;
        it does not acknowledge payment or remove physical units.
        """
        if self.status is not OrderStatus.PENDING:
            raise InvalidOrderTransitionError(
                "terminal orders cannot be resolved again"
            )
        if status is OrderStatus.PENDING:
            raise InvalidOrderTransitionError("resolution must target a terminal state")
        return replace(self, status=status, resolved_at=at, reason=reason)
