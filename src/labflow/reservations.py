"""Immutable order-linked reservation lifecycle proposals, without stock writes."""

from dataclasses import dataclass, replace
from datetime import datetime
from enum import StrEnum
from typing import Self
from uuid import UUID

from labflow.orders import Order, OrderItem
from labflow.validation import normalize_utc_datetime


class ReservationStatus(StrEnum):
    """Held units can be committed or released; both outcomes are terminal."""

    HELD = "held"
    COMMITTED = "committed"
    RELEASED = "released"


class InvalidReservationTransitionError(ValueError):
    """A transition would reopen or replace a terminal reservation outcome."""


@dataclass(frozen=True, slots=True, kw_only=True)
class Reservation:
    """An immutable hold tied to all items of one accepted order snapshot.

    Args:
        reservation_id: Server-assigned UUID identifying the reservation independently of the order.
        order: Accepted immutable Order supplying order/store identity, acceptance
            time and frozen items. This reference is not the live database state.
        expires_at: Offset-aware pending-hold deadline, strictly after order
            acceptance and normalized to UTC. No automatic clock is consulted.
        status: ReservationStatus member, defaulting to HELD. Raw strings fail.
        resolved_at: Offset-aware resolution time, normalized to UTC. Absent for
            HELD, required for terminal states and never before acceptance.
            COMMITTED requires a time strictly before expires_at.

    Raises:
        TypeError: Identity, order, status or timestamp has an invalid type.
        ValueError: Deadline, resolution time or lifecycle metadata is invalid.

    Construction does not reserve stock or enforce one reservation per order.
    Store and items derive from the frozen order, without a second editable copy.
    Future persistence will store an order foreign key and guard current order,
    reservation and inventory state atomically. Local proposals do not provide
    locking, idempotency, deduplication, authorization or durable expiry.
    """

    reservation_id: UUID
    order: Order
    expires_at: datetime
    status: ReservationStatus = ReservationStatus.HELD
    resolved_at: datetime | None = None

    def __post_init__(self) -> None:
        """Validate the immutable order link, deadline and resolution metadata."""
        if not isinstance(self.reservation_id, UUID):
            raise TypeError("reservation_id must be a UUID")
        if not isinstance(self.order, Order):
            raise TypeError("order must be an Order")
        if not isinstance(self.status, ReservationStatus):
            raise TypeError("status must be a ReservationStatus")
        expires_at = normalize_utc_datetime(self.expires_at, "expires_at")
        if expires_at <= self.order.accepted_at:
            raise ValueError("expires_at must follow order acceptance")
        object.__setattr__(self, "expires_at", expires_at)
        if self.status is ReservationStatus.HELD:
            if self.resolved_at is not None:
                raise ValueError("held reservations cannot have resolved_at")
        else:
            resolved_at = normalize_utc_datetime(self.resolved_at, "resolved_at")
            if resolved_at < self.order.accepted_at:
                raise ValueError("resolved_at must not precede order acceptance")
            if self.status is ReservationStatus.COMMITTED and resolved_at >= expires_at:
                raise ValueError("cannot commit an expired reservation")
            object.__setattr__(self, "resolved_at", resolved_at)

    @property
    def order_id(self) -> UUID:
        """Return the linked order identifier for future persistence lookup."""
        return self.order.order_id

    @property
    def store_id(self) -> str:
        """Return the store from the accepted quote."""
        return self.order.quote.store_id

    @property
    def items(self) -> tuple[OrderItem, ...]:
        """Return the frozen SKUs and quantities, excluding pricing metadata."""
        return tuple(
            OrderItem(item.product_id, item.quantity) for item in self.order.quote.items
        )

    def commit(self, *, at: datetime) -> Self:
        """Propose committing an unexpired hold for fulfillment.

        Args:
            at: Trusted offset-aware time at or after acceptance and strictly
                before expiry. Future persistence must use database time.

        Returns:
            A new COMMITTED reservation retaining identity, order and deadline.
            The original remains unchanged; no physical units are removed.

        Raises:
            InvalidReservationTransitionError: This reservation is terminal.
            TypeError: Time is not a datetime.
            ValueError: Time is naive, before acceptance or at/after expiry.

        Current order/attempt/deadline guards and the associated order confirmation
        must commit in one future database transaction. This reference to the
        accepted order is not advanced or treated as current database state.
        """
        return self._resolve(ReservationStatus.COMMITTED, at=at)

    def release(self, *, at: datetime) -> Self:
        """Propose releasing a hold after rejection or definitive technical failure.

        Args:
            at: Trusted offset-aware time at or after order acceptance.
                Release is allowed before or after the pending-hold deadline.

        Returns:
            A new RELEASED reservation, without modifying order or stock counts.

        Raises:
            InvalidReservationTransitionError: This reservation is terminal.
            TypeError: Time is not a datetime.
            ValueError: Time is naive or before acceptance.
        """
        return self._resolve(ReservationStatus.RELEASED, at=at)

    def expire(self, *, at: datetime) -> Self:
        """Propose releasing a held reservation whose deadline has been reached.

        Args:
            at: Trusted offset-aware time at or after expires_at.

        Returns:
            A new RELEASED reservation. Expiry is a release cause, not a state.

        Raises:
            InvalidReservationTransitionError: This reservation is terminal.
            TypeError: Time is not a datetime.
            ValueError: Time is naive or before the deadline.

        Future recovery must atomically fail the pending order and release stock.
        Committed reservations never expire through this pending-order operation.
        """
        return self._resolve(ReservationStatus.RELEASED, at=at, require_expired=True)

    def _resolve(
        self,
        status: ReservationStatus,
        *,
        at: datetime,
        require_expired: bool = False,
    ) -> Self:
        """Build a terminal proposal through one shared transition guard.

        Args:
            status: Terminal target selected by commit, release or expire.
            at: Trusted resolution time, validated by the resulting snapshot.
            require_expired: Require the deadline to be reached for expiry only.

        Returns:
            A new validated snapshot, preserving the original.

        Raises:
            InvalidReservationTransitionError: The reservation is already terminal.
            TypeError: Resolution time has an unsupported type.
            ValueError: Timestamp or deadline requirements are violated.
        """
        if self.status is not ReservationStatus.HELD:
            raise InvalidReservationTransitionError(
                "terminal reservations cannot be resolved again"
            )
        if require_expired:
            at = normalize_utc_datetime(at, "resolved_at")
            if at < self.expires_at:
                raise ValueError("cannot expire a reservation before its deadline")
        return replace(self, status=status, resolved_at=at)
