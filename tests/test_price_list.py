"""Trusted price list metadata, price validation and snapshot isolation."""

import json
from dataclasses import FrozenInstanceError

import pytest

from labflow.currency import MAX_AMOUNT_CENTS, Currency
from labflow.price_list import PriceList
from labflow.store import Store


@pytest.fixture(name="price_list")
def price_list_fixture(make_price_list):
    """Provide a fictional server-controlled price list in EUR."""
    return make_price_list(
        store=Store("store-a", Currency.EUR),
        version="prices-v1",
        prices={"camiseta": 1990, "caneca": 3500},
    )


def test_price_list_records_store_currency_version_and_prices(price_list):
    """Keep the complete pricing context in one snapshot."""
    assert price_list.store_id == "store-a"
    assert price_list.currency is Currency.EUR
    assert price_list.version == "prices-v1"
    assert dict(price_list.prices) == {"camiseta": 1990, "caneca": 3500}


def test_price_list_copies_source_mapping(make_price_list):
    """Later source edits must not change an already constructed price list."""
    source = {"camiseta": 1990}
    price_list = make_price_list(Store("store-a", Currency.EUR), "prices-v1", source)

    source["camiseta"] = 2500
    source["caderno"] = 1200

    assert dict(price_list.prices) == {"camiseta": 1990}


def test_price_list_prices_are_read_only(price_list):
    """Prevent changing prices through the price list's public mapping."""
    with pytest.raises(TypeError):
        price_list.prices["camiseta"] = 2500


@pytest.mark.parametrize("field", ["store", "version", "prices", "catalog"])
def test_price_list_fields_are_frozen(price_list, field):
    """Prevent replacing metadata or bypassing the read-only price view."""
    with pytest.raises(FrozenInstanceError):
        setattr(price_list, field, "changed")


@pytest.mark.parametrize("field", ["version"])
@pytest.mark.parametrize("value", ["", " ", " padded "])
def test_rejects_invalid_price_list_identifiers(make_price_list, field, value):
    """Reject ambiguous store and snapshot identifiers."""
    parameters = {
        "store": Store("store-a", Currency.EUR),
        "version": "prices-v1",
        "prices": {"camiseta": 1990},
    }
    parameters[field] = value
    with pytest.raises(ValueError, match=field):
        make_price_list(**parameters)


@pytest.mark.parametrize("field", ["store", "version"])
def test_rejects_invalid_price_list_metadata_types(make_price_list, field):
    """Annotations do not replace runtime validation of price list metadata."""
    parameters = {
        "store": Store("store-a", Currency.EUR),
        "version": "prices-v1",
        "prices": {},
    }
    parameters[field] = None
    with pytest.raises(TypeError, match=field):
        make_price_list(**parameters)


@pytest.mark.parametrize("currency", ["USD", "eur", "", "JPY"])
def test_rejects_unsupported_currency(currency):
    """Unsupported or malformed codes are rejected before store construction."""
    with pytest.raises(ValueError):
        Currency(currency)


@pytest.mark.parametrize("prices", [None, [], "1990"])
def test_rejects_non_mapping_prices(make_price_list, prices):
    """Require a product-to-price mapping at the price list boundary."""
    with pytest.raises(TypeError, match="prices must be a mapping"):
        make_price_list(Store("store-a", Currency.EUR), "prices-v1", prices)


@pytest.mark.parametrize("product_id", ["", " ", " camiseta "])
def test_rejects_invalid_price_list_product_identifiers(make_price_list, product_id):
    """Validate every SKU, including products not requested by an order."""
    with pytest.raises(ValueError, match="product_id"):
        make_price_list(Store("store-a", Currency.EUR), "prices-v1", {product_id: 1990})


def test_rejects_non_string_price_list_product_identifier(make_price_list):
    """Require the same identifier type used in buyer requests."""
    with pytest.raises(TypeError, match="product_id"):
        make_price_list(Store("store-a", Currency.EUR), "prices-v1", {1: 1990})


@pytest.mark.parametrize("price", [False, "1990", None])
def test_rejects_non_integer_price_list_prices(make_price_list, price):
    """Never treat booleans or formatted amounts as integer cents."""
    with pytest.raises(TypeError, match="unit_price_cents"):
        make_price_list(
            Store("store-a", Currency.EUR), "prices-v1", {"camiseta": price}
        )


def test_rejects_price_list_price_above_supported_range(make_price_list):
    """Reject an amount outside the range planned for persisted cents."""
    with pytest.raises(ValueError, match="unit_price_cents"):
        make_price_list(
            Store("store-a", Currency.EUR),
            "prices-v1",
            {"camiseta": MAX_AMOUNT_CENTS + 1},
        )


def test_accepts_price_list_price_boundaries(make_price_list):
    """Zero-priced products and the maximum unit price are valid price list values."""
    price_list = make_price_list(
        Store("store-a", Currency.EUR),
        "prices-v1",
        {"free": 0, "maximum": MAX_AMOUNT_CENTS},
    )
    assert dict(price_list.prices) == {"free": 0, "maximum": MAX_AMOUNT_CENTS}


def test_accepts_empty_price_list(make_price_list):
    """A store may have no products; order lookup still rejects unknown SKUs."""
    assert not make_price_list(Store("store-a", Currency.EUR), "prices-v1", {}).prices


@pytest.mark.parametrize(
    "overrides", [{"currency": Currency.BRL}, {"store_id": "other-store"}]
)
def test_rejects_independent_price_list_metadata(make_price_list, overrides):
    """A caller cannot override identity or currency from the bound store."""
    with pytest.raises(TypeError):
        make_price_list(
            store=Store("store-a", Currency.EUR),
            version="prices-v1",
            prices={},
            **overrides,
        )


def test_currency_round_trips_as_a_code_in_json(price_list):
    """JSON boundaries retain the currency code and can parse it back explicitly."""
    payload = json.loads(json.dumps({"currency": price_list.currency}))

    assert payload == {"currency": "EUR"}
    assert Currency(payload["currency"]) is Currency.EUR


def test_price_lists_derive_currency_and_prices_from_their_store(
    make_price_list, berlin_store, saopaulo_store
):
    """The same SKU has independent prices in two store currencies."""
    berlin = make_price_list(berlin_store, "prices-v1", {"sku-shirt": 1990})
    saopaulo = make_price_list(saopaulo_store, "prices-v1", {"sku-shirt": 5990})

    assert berlin.store is berlin_store
    assert berlin.store_id == "store-berlin"
    assert berlin.currency is Currency.EUR
    assert berlin.prices["sku-shirt"] == 1990
    assert saopaulo.store is saopaulo_store
    assert saopaulo.store_id == "store-saopaulo"
    assert saopaulo.currency is Currency.BRL
    assert saopaulo.prices["sku-shirt"] == 5990


def test_rejects_invalid_product_catalog(berlin_store):
    """A price list must reference a validated product snapshot."""
    with pytest.raises(TypeError, match="catalog must be a Catalog instance"):
        PriceList(berlin_store, "prices-v1", {}, None)


@pytest.mark.parametrize(
    "product_id, error", [(None, TypeError), ("", ValueError), (" padded ", ValueError)]
)
def test_rejects_invalid_price_keys_against_valid_catalog(
    shared_catalog, berlin_store, product_id, error
):
    """Validate offered SKU identifiers independently of catalog construction."""
    with pytest.raises(error, match="product_id"):
        PriceList(berlin_store, "prices-v1", {product_id: 1990}, shared_catalog)
