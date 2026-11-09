"""Inventory snapshot invariants, availability and store/SKU identity."""

from dataclasses import FrozenInstanceError, replace

import pytest

from labflow.inventory import MAX_STOCK_UNITS, InventorySnapshot


@pytest.fixture(name="balance")
def balance_fixture(berlin_store, shared_catalog):
    """Provide a snapshot of ten shirts without reserved units."""
    return InventorySnapshot(
        store=berlin_store, product=shared_catalog.products["sku-shirt"], on_hand=10
    )


@pytest.mark.parametrize(
    "on_hand,reserved,available",
    [
        (10, 0, 10),
        (10, 6, 4),
        (10, 10, 0),
        (0, 0, 0),
        (MAX_STOCK_UNITS, 0, MAX_STOCK_UNITS),
        (MAX_STOCK_UNITS, MAX_STOCK_UNITS, 0),
    ],
)
def test_available_stock_is_derived(balance, on_hand, reserved, available):
    """Represent empty, partial, full and maximum counts without updating stock."""
    snapshot = replace(balance, on_hand=on_hand, reserved=reserved)
    assert snapshot.available == available


def test_snapshot_exposes_store_and_product_identity(balance):
    """Retain the configured store and SKU identifiers."""
    assert (balance.store_id, balance.product_id) == ("store-berlin", "sku-shirt")


def test_shared_product_has_independent_store_snapshots(balance, saopaulo_store):
    """Shared metadata does not combine quantities across stores."""
    other = replace(balance, store=saopaulo_store, on_hand=20, reserved=3)
    assert other.product is balance.product
    assert other.store_id != balance.store_id
    assert (balance.available, other.available) == (10, 17)


def test_same_store_has_separate_product_snapshots(balance, shared_catalog):
    """Each SKU carries its own counts."""
    other = replace(balance, product=shared_catalog.products["sku-mug"], on_hand=4)
    assert other.store is balance.store
    assert other.product_id != balance.product_id
    assert (balance.available, other.available) == (10, 4)


@pytest.mark.parametrize("field", ["store", "product", "on_hand", "reserved"])
def test_balance_fields_are_frozen(balance, field):
    """Prevent assignments from bypassing balance invariants."""
    with pytest.raises(FrozenInstanceError):
        setattr(balance, field, None)


@pytest.mark.parametrize("field", ["on_hand", "reserved"])
@pytest.mark.parametrize("value", [True, False, 1.5, "1", None])
def test_rejects_non_integer_stock_counts(balance, field, value):
    """Stock quantities use exact integer unit counts."""
    with pytest.raises(TypeError, match=field):
        replace(balance, **{field: value})


@pytest.mark.parametrize("field", ["on_hand", "reserved"])
@pytest.mark.parametrize("value", [-1, MAX_STOCK_UNITS + 1])
def test_rejects_stock_counts_outside_supported_range(balance, field, value):
    """Reject invalid counts before deriving available stock."""
    with pytest.raises(ValueError, match=field):
        replace(balance, **{field: value})


def test_rejects_reserved_units_exceeding_on_hand(balance):
    """The constructor cannot create negative availability."""
    with pytest.raises(ValueError, match="reserved must not exceed on_hand"):
        replace(balance, reserved=11)


@pytest.mark.parametrize("field", ["store", "product"])
def test_rejects_invalid_balance_references(balance, field):
    """Require the same domain types already used by catalogs and price lists."""
    with pytest.raises(TypeError, match=field):
        replace(balance, **{field: None})
