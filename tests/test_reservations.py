"""Reservation deadlines, terminal transitions and frozen order linkage."""

from dataclasses import FrozenInstanceError, replace
from datetime import UTC, datetime, timedelta, timezone
from uuid import UUID

import pytest

from labflow.orders import Order, OrderItem, OrderQuote, OrderStatus
from labflow.reservations import (
    InvalidReservationTransitionError,
    Reservation,
    ReservationStatus,
)


@pytest.fixture(name="held")
def held_reservation_fixture(make_price_list):
    """Link a five-minute hold to an accepted order containing two SKUs."""
    order = Order(
        order_id=UUID("00000000-0000-4000-8000-000000000001"),
        buyer_id="buyer-a",
        quote=OrderQuote.from_price_list(
            items=(OrderItem("sku-shirt", 2), OrderItem("sku-mug", 1)),
            price_list=make_price_list(prices={"sku-shirt": 1990, "sku-mug": 3500}),
        ),
        accepted_at=datetime(2026, 11, 10, 10, tzinfo=UTC),
    )
    return Reservation(
        reservation_id=UUID("00000000-0000-4000-8000-000000000002"),
        order=order,
        expires_at=order.accepted_at + timedelta(minutes=5),
    )


def test_new_reservation_is_held_and_identifies_all_order_items(held):
    """No second editable copy of store, SKU or quantity can diverge."""
    assert held.status is ReservationStatus.HELD
    assert held.resolved_at is None
    assert held.order_id == held.order.order_id
    assert held.store_id == held.order.quote.store_id
    assert held.items == (OrderItem("sku-shirt", 2), OrderItem("sku-mug", 1))


@pytest.mark.parametrize("source", list(ReservationStatus))
@pytest.mark.parametrize("operation", ["commit", "release", "expire"])
def test_resolution_matrix_including_repeated_operations(held, source, operation):
    """Terminal outcomes cannot be repeated, replaced or reopened."""
    current = held
    if source is ReservationStatus.COMMITTED:
        current = held.commit(at=held.order.accepted_at)
    elif source is ReservationStatus.RELEASED:
        current = held.release(at=held.order.accepted_at)
    at = held.expires_at if operation == "expire" else held.order.accepted_at
    if source is not ReservationStatus.HELD:
        with pytest.raises(InvalidReservationTransitionError):
            getattr(current, operation)(at=at)
    else:
        updated = getattr(current, operation)(at=at)
        expected = (
            ReservationStatus.COMMITTED
            if operation == "commit"
            else ReservationStatus.RELEASED
        )
        assert updated.status is expected
        assert updated.resolved_at == at
        assert updated.reservation_id == current.reservation_id
        assert updated.order is current.order
        assert updated.items == current.items
        assert updated.expires_at == current.expires_at
    assert current.status is source
    assert held.status is ReservationStatus.HELD
    assert held.order.status is OrderStatus.PENDING


@pytest.mark.parametrize("offset", [timedelta(0), timedelta(seconds=1)])
def test_commit_refuses_reached_or_passed_deadline(held, offset):
    """The deadline is exclusive for commitment, including the exact instant."""
    with pytest.raises(ValueError, match="cannot commit an expired reservation"):
        held.commit(at=held.expires_at + offset)
    assert held.resolved_at is None


def test_commit_accepts_last_instant_before_deadline(held):
    """A still-valid hold can be committed immediately before expiry."""
    at = held.expires_at - timedelta(microseconds=1)
    assert held.commit(at=at).resolved_at == at


@pytest.mark.parametrize("offset", [timedelta(0), timedelta(seconds=1)])
def test_expiry_releases_at_or_after_deadline(held, offset):
    """Delayed recovery can release an overdue hold."""
    updated = held.expire(at=held.expires_at + offset)
    assert updated.status is ReservationStatus.RELEASED
    assert updated.resolved_at == held.expires_at + offset


def test_expiry_refuses_last_instant_before_deadline(held):
    """Recovery cannot reclaim a still-valid hold."""
    with pytest.raises(ValueError, match="before its deadline"):
        held.expire(at=held.expires_at - timedelta(microseconds=1))
    assert held.status is ReservationStatus.HELD


@pytest.mark.parametrize("late", [False, True])
def test_committed_hold_never_expires(held, late):
    """Confirmed commitments are excluded from pending-order recovery."""
    committed = held.commit(at=held.order.accepted_at)
    at = held.expires_at + (timedelta(days=1) if late else timedelta(0))
    with pytest.raises(InvalidReservationTransitionError):
        committed.expire(at=at)
    assert committed.status is ReservationStatus.COMMITTED


@pytest.mark.parametrize("operation", ["commit", "release"])
def test_resolution_refuses_time_before_acceptance(held, operation):
    """Resolution cannot precede the order that established the hold."""
    with pytest.raises(ValueError, match="must not precede order acceptance"):
        getattr(held, operation)(at=held.order.accepted_at - timedelta(microseconds=1))


@pytest.mark.parametrize("at", [None, True, "2026-11-10T10:00:00Z", 1])
@pytest.mark.parametrize("operation", ["commit", "release", "expire"])
def test_resolution_refuses_non_datetime_values(held, at, operation):
    """Require trusted timestamps without coercing external values."""
    with pytest.raises(TypeError, match="resolved_at must be a datetime"):
        getattr(held, operation)(at=at)


@pytest.mark.parametrize("operation", ["commit", "release", "expire"])
def test_resolution_refuses_naive_time(held, operation):
    """A missing timezone is never guessed from the machine."""
    with pytest.raises(ValueError, match="resolved_at must be offset-aware"):
        getattr(held, operation)(at=datetime(2026, 11, 10, 10))


@pytest.mark.parametrize("value", [None, True, "2026-11-10T10:05:00Z", 1])
def test_deadline_requires_datetime(held, value):
    """Validate timestamp types before comparing deadlines."""
    with pytest.raises(TypeError, match="expires_at must be a datetime"):
        replace(held, expires_at=value)


def test_deadline_refuses_naive_time(held):
    """The deadline has the same explicit offset contract as resolution."""
    with pytest.raises(ValueError, match="expires_at must be offset-aware"):
        replace(held, expires_at=datetime(2026, 11, 10, 10, 5))


@pytest.mark.parametrize("offset", [timedelta(0), timedelta(microseconds=-1)])
def test_deadline_must_follow_acceptance(held, offset):
    """A new hold needs a nonempty time window."""
    with pytest.raises(ValueError, match="must follow order acceptance"):
        replace(held, expires_at=held.order.accepted_at + offset)


def test_offset_times_are_normalized_to_utc(held):
    """Deadline and resolution represent instants independent of local timezone."""
    offset = timezone(timedelta(hours=-3))
    current = replace(held, expires_at=held.expires_at.astimezone(offset))
    updated = current.commit(at=held.order.accepted_at.astimezone(offset))
    assert current.expires_at.tzinfo is UTC
    assert current.expires_at == held.expires_at
    assert updated.resolved_at.tzinfo is UTC
    assert updated.resolved_at == held.order.accepted_at


@pytest.mark.parametrize(
    "status", [ReservationStatus.COMMITTED, ReservationStatus.RELEASED]
)
def test_terminal_rehydration_requires_resolution_time(held, status):
    """Loaded terminal snapshots cannot omit their decision timestamp."""
    with pytest.raises(TypeError, match="resolved_at must be a datetime"):
        replace(held, status=status)


def test_held_snapshot_cannot_have_resolution_time(held):
    """Reject contradictory lifecycle metadata during construction."""
    with pytest.raises(ValueError, match="held reservations cannot have resolved_at"):
        replace(held, resolved_at=held.order.accepted_at)


def test_loaded_commit_cannot_bypass_deadline(held):
    """Direct construction enforces the same time boundary as commit."""
    with pytest.raises(ValueError, match="cannot commit an expired reservation"):
        replace(held, status=ReservationStatus.COMMITTED, resolved_at=held.expires_at)


@pytest.mark.parametrize(
    "field,value",
    [
        ("reservation_id", "00000000-0000-4000-8000-000000000002"),
        ("order", None),
        ("status", "held"),
    ],
)
def test_invalid_reference_and_status_types_are_rejected(held, field, value):
    """UUID, order and enum boundaries remain explicit."""
    with pytest.raises(TypeError, match=field):
        replace(held, **{field: value})


@pytest.mark.parametrize(
    "field", ["reservation_id", "order", "expires_at", "status", "resolved_at"]
)
def test_reservation_fields_are_immutable(held, field):
    """Direct assignment cannot bypass transition rules."""
    with pytest.raises(FrozenInstanceError):
        setattr(held, field, None)


def test_release_does_not_require_deadline_expiration(held):
    """Rejection or definitive failure may free a hold before its deadline."""
    released = held.release(at=held.order.accepted_at)
    assert released.status is ReservationStatus.RELEASED
    assert released.resolved_at < released.expires_at


def test_release_after_deadline_is_allowed(held):
    """An overdue pending hold can also be released by a terminal failure."""
    assert held.release(at=held.expires_at).status is ReservationStatus.RELEASED


def test_new_identity_does_not_reopen_previous_reservation(held):
    """A separately constructed proposal leaves the original history unchanged."""
    previous = held.release(at=held.order.accepted_at)
    new = replace(held, reservation_id=UUID("00000000-0000-4000-8000-000000000003"))
    assert new.reservation_id != previous.reservation_id
    assert previous.status is ReservationStatus.RELEASED
    assert new.status is ReservationStatus.HELD


@pytest.mark.parametrize(
    "accepted,deadline,before_deadline",
    [
        (
            "2020-10-25T02:58:00+02:00",
            "2020-10-25T02:03:00+01:00",
            "2020-10-25T02:59:00+02:00",
        ),
        (
            "2020-03-29T01:58:00+01:00",
            "2020-03-29T03:03:00+02:00",
            "2020-03-29T01:59:00+01:00",
        ),
    ],
)
def test_reservation_deadline_uses_elapsed_time_across_dst(
    held, accepted, deadline, before_deadline
):
    """Commit and expiry agree on a five-minute window despite local clock jumps."""
    order = replace(held.order, accepted_at=datetime.fromisoformat(accepted))
    current = replace(held, order=order, expires_at=datetime.fromisoformat(deadline))
    assert current.expires_at - order.accepted_at == timedelta(minutes=5)
    assert (
        current.commit(at=datetime.fromisoformat(before_deadline)).status
        is ReservationStatus.COMMITTED
    )
    assert (
        current.expire(at=datetime.fromisoformat(deadline)).status
        is ReservationStatus.RELEASED
    )
    with pytest.raises(ValueError, match="cannot commit an expired reservation"):
        current.commit(at=datetime.fromisoformat(deadline))
