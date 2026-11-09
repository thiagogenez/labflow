"""Price snapshots and admission bounds for the pure order calculation."""

from dataclasses import FrozenInstanceError

import pytest

from labflow.currency import Currency
from labflow.orders import OrderItem, OrderQuote, QuotedItem
from labflow.price_list import PriceList
from labflow.store import Store


def test_quote_known_order(make_price_list):
    """Use server price list prices to calculate the expected basket amount."""
    quote = OrderQuote.from_price_list(
        items=(OrderItem("camiseta", 2), OrderItem("caneca", 1)),
        price_list=make_price_list(
            store=Store("store-a", Currency.EUR),
            version="prices-v1",
            prices={"camiseta": 1990, "caneca": 3500},
        ),
    )

    assert quote.store_id == "store-a"
    assert quote.currency is Currency.EUR
    assert quote.catalog_version == "products-v1"
    assert quote.price_list_version == "prices-v1"
    assert quote.items[0].unit_price_cents == 1990
    assert quote.items[0].total_cents == 3980
    assert quote.total_cents == 7480


def test_quote_keeps_prices_after_price_list_changes(make_price_list):
    """A later price list edit affects new quotes only."""
    price_list = {"camiseta": 1990}
    items = (OrderItem("camiseta", 2),)
    original = OrderQuote.from_price_list(
        items=items,
        price_list=make_price_list(
            store=Store("store-a", Currency.EUR),
            version="prices-v1",
            prices=price_list,
        ),
    )
    price_list["camiseta"] = 2500
    updated = OrderQuote.from_price_list(
        items=items,
        price_list=make_price_list(
            store=Store("store-a", Currency.EUR),
            version="prices-v2",
            prices=price_list,
        ),
    )

    assert original.total_cents == 3980
    assert original.price_list_version == "prices-v1"
    assert updated.total_cents == 5000
    with pytest.raises(FrozenInstanceError):
        original.items[0].unit_price_cents = 2500


@pytest.mark.parametrize("quantity", [True, False, 1.5, "2", None])
def test_rejects_non_integer_quantities(quantity):
    """Annotations alone do not enforce runtime input types."""
    with pytest.raises(TypeError, match="quantity must be an integer"):
        OrderItem("camiseta", quantity)


@pytest.mark.parametrize("quantity", [0, -1, 101])
def test_rejects_quantities_outside_budget(quantity):
    """Bound one requested product quantity before doing work."""
    with pytest.raises(ValueError, match="quantity must be between"):
        OrderItem("camiseta", quantity)


@pytest.mark.parametrize("product_id", ["", " ", " camiseta"])
def test_rejects_invalid_product_identifiers(product_id):
    """Do not silently normalize identifiers used for product lookup."""
    with pytest.raises(ValueError, match="product_id must be nonempty"):
        OrderItem(product_id, 1)


def test_rejects_unknown_product(make_price_list):
    """A missing SKU must not acquire a random or default price."""
    with pytest.raises(ValueError, match="product not offered by store: caneca"):
        OrderQuote.from_price_list(
            items=(OrderItem("caneca", 1),),
            price_list=make_price_list(
                store=Store("store-a", Currency.EUR),
                version="prices-v1",
                prices={"camiseta": 1990},
            ),
        )


@pytest.mark.parametrize("rows", [0, 21])
def test_rejects_line_counts_outside_budget(make_price_list, rows):
    """Reject oversized baskets before consulting price list prices."""
    with pytest.raises(ValueError, match="orders must contain"):
        OrderQuote.from_price_list(
            items=tuple(OrderItem(str(index), 1) for index in range(rows)),
            price_list=make_price_list(
                store=Store("store-a", Currency.EUR),
                version="prices-v1",
                prices={},
            ),
        )


def test_rejects_duplicate_product_lines(make_price_list):
    """Do not bypass per-line quantity bounds using repeated product lines."""
    with pytest.raises(ValueError, match="duplicate product lines"):
        OrderQuote.from_price_list(
            items=(OrderItem("camiseta", 60), OrderItem("camiseta", 60)),
            price_list=make_price_list(
                store=Store("store-a", Currency.EUR),
                version="prices-v1",
                prices={"camiseta": 1990},
            ),
        )


@pytest.mark.parametrize(
    "price, error", [(True, TypeError), (1.5, TypeError), (-1, ValueError)]
)
def test_rejects_invalid_price_list_prices(make_price_list, price, error):
    """A trusted source must still provide valid integer-cent amounts."""
    with pytest.raises(error, match="unit_price_cents"):
        OrderQuote.from_price_list(
            items=(OrderItem("camiseta", 1),),
            price_list=make_price_list(
                store=Store("store-a", Currency.EUR),
                version="prices-v1",
                prices={"camiseta": price},
            ),
        )


def test_rejects_total_outside_supported_amount_range(make_price_list):
    """Python integers can exceed the range intended for persisted amounts."""
    with pytest.raises(ValueError, match="order total exceeds"):
        OrderQuote.from_price_list(
            items=(OrderItem("camiseta", 2),),
            price_list=make_price_list(
                store=Store("store-a", Currency.EUR),
                version="prices-v1",
                prices={"camiseta": 2**63 - 1},
            ),
        )


def test_quotes_keep_each_price_lists_store_and_version(make_price_list):
    """Identical SKU identifiers in different stores retain their pricing origin."""
    items = (OrderItem("camiseta", 2),)
    first = OrderQuote.from_price_list(
        items=items,
        price_list=make_price_list(
            Store("store-a", Currency.EUR), "prices-v1", {"camiseta": 1990}
        ),
    )
    second = OrderQuote.from_price_list(
        items=items,
        price_list=make_price_list(
            Store("store-b", Currency.EUR), "prices-v2", {"camiseta": 2500}
        ),
    )

    assert (first.store_id, first.currency, first.price_list_version) == (
        "store-a",
        Currency.EUR,
        "prices-v1",
    )
    assert (second.store_id, second.currency, second.price_list_version) == (
        "store-b",
        Currency.EUR,
        "prices-v2",
    )
    assert first.total_cents == 3980
    assert second.total_cents == 5000


def test_rejects_bare_price_mapping_instead_of_price_list():
    """Require prices and their metadata together instead of unrelated arguments."""
    with pytest.raises(TypeError, match="price_list must be a PriceList instance"):
        OrderQuote.from_price_list(
            items=(OrderItem("camiseta", 1),), price_list={"camiseta": 1990}
        )


def test_rejects_raw_currency_string_in_direct_quote():
    """Direct quote construction enforces the same domain type as price lists."""
    with pytest.raises(TypeError, match="currency must be a Currency member"):
        OrderQuote(
            store_id="store-a",
            currency="EUR",
            catalog_version="products-v1",
            price_list_version="prices-v1",
            items=(QuotedItem("camiseta", 1, 1990),),
        )


def test_quotes_preserve_each_stores_currency_without_conversion(
    make_price_list, berlin_store, saopaulo_store
):
    """One request shape is quoted separately against each store's own price list."""
    items = (OrderItem("sku-shirt", 2),)
    berlin_catalog = make_price_list(berlin_store, "prices-v1", {"sku-shirt": 1990})
    saopaulo_catalog = make_price_list(saopaulo_store, "prices-v1", {"sku-shirt": 5990})

    berlin = OrderQuote.from_price_list(items=items, price_list=berlin_catalog)
    saopaulo = OrderQuote.from_price_list(items=items, price_list=saopaulo_catalog)

    assert berlin.store_id == "store-berlin"
    assert berlin.currency is Currency.EUR
    assert berlin.total_cents == 3980
    assert saopaulo.store_id == "store-saopaulo"
    assert saopaulo.currency is Currency.BRL
    assert saopaulo.total_cents == 11980


def test_quotes_share_metadata_but_use_independent_store_prices(
    shared_catalog, berlin_store, saopaulo_store
):
    """The same catalog object produces EUR/BRL quotes from separate price lists."""
    berlin_prices = PriceList(
        berlin_store, "berlin-v1", {"sku-shirt": 1990}, shared_catalog
    )
    saopaulo_prices = PriceList(
        saopaulo_store, "saopaulo-v3", {"sku-shirt": 5990}, shared_catalog
    )
    items = (OrderItem("sku-shirt", 2),)

    berlin = OrderQuote.from_price_list(items=items, price_list=berlin_prices)
    saopaulo = OrderQuote.from_price_list(items=items, price_list=saopaulo_prices)

    assert berlin_prices.catalog is saopaulo_prices.catalog
    assert berlin.catalog_version == saopaulo.catalog_version == "products-v1"
    assert (
        berlin.store_id,
        berlin.currency,
        berlin.price_list_version,
        berlin.total_cents,
    ) == ("store-berlin", Currency.EUR, "berlin-v1", 3980)
    assert (
        saopaulo.store_id,
        saopaulo.currency,
        saopaulo.price_list_version,
        saopaulo.total_cents,
    ) == ("store-saopaulo", Currency.BRL, "saopaulo-v3", 11980)


def test_product_in_catalog_is_not_necessarily_offered_by_store(
    shared_catalog, berlin_store
):
    """Shared metadata alone does not provide a selling price in every store."""
    prices = PriceList(berlin_store, "prices-v1", {"sku-shirt": 1990}, shared_catalog)
    with pytest.raises(ValueError, match="product not offered by store: sku-mug"):
        OrderQuote.from_price_list(items=(OrderItem("sku-mug", 1),), price_list=prices)
