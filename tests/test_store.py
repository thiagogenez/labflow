"""Immutable store configuration and explicit supported currencies."""

from dataclasses import FrozenInstanceError

import pytest

from labflow.currency import Currency
from labflow.store import Store


def test_stores_have_their_own_fixed_currency(berlin_store, saopaulo_store):
    """Each fixture has exactly one configured pricing currency."""
    assert berlin_store.store_id == "store-berlin"
    assert berlin_store.currency is Currency.EUR
    assert saopaulo_store.store_id == "store-saopaulo"
    assert saopaulo_store.currency is Currency.BRL


@pytest.mark.parametrize("field", ["store_id", "currency"])
def test_store_configuration_is_frozen(berlin_store, field):
    """Prevent changing identity or currency through a retained store reference."""
    with pytest.raises(FrozenInstanceError):
        setattr(berlin_store, field, "changed")


@pytest.mark.parametrize("store_id", ["", " ", " padded "])
def test_rejects_invalid_store_identifier(store_id):
    """Reject empty or ambiguous store identifiers."""
    with pytest.raises(ValueError, match="store_id"):
        Store(store_id, Currency.EUR)


def test_rejects_non_string_store_identifier():
    """Store identity uses the same string type as accepted-order metadata."""
    with pytest.raises(TypeError, match="store_id"):
        Store(None, Currency.EUR)


@pytest.mark.parametrize("currency", ["EUR", "BRL", None])
def test_rejects_currency_that_is_not_an_enum_member(currency):
    """Domain constructors require explicit parsed currency members."""
    with pytest.raises(TypeError, match="currency must be a Currency member"):
        Store("store-a", currency)


@pytest.mark.parametrize("currency", [Currency.EUR, Currency.BRL])
def test_supported_currency_codes_round_trip(currency):
    """Both supported currencies have an explicit external code."""
    assert Currency(currency.value) is currency
