"""Fictional store configurations shared by pricing-domain tests."""

import pytest

from labflow.catalog import Catalog, Product
from labflow.currency import Currency
from labflow.price_list import PriceList
from labflow.store import Store


@pytest.fixture(name="berlin_store")
def berlin_store_fixture() -> Store:
    """Provide a server-controlled EUR store."""
    return Store(store_id="store-berlin", currency=Currency.EUR)


@pytest.fixture(name="saopaulo_store")
def saopaulo_store_fixture() -> Store:
    """Provide a server-controlled BRL store."""
    return Store(store_id="store-saopaulo", currency=Currency.BRL)


@pytest.fixture(name="shared_catalog")
def shared_catalog_fixture() -> Catalog:
    """Provide product metadata shared by both stores without price or stock."""
    return Catalog(
        version="products-v1",
        products={
            "sku-shirt": Product("sku-shirt", "Black T-shirt, size M", "Cotton"),
            "sku-mug": Product("sku-mug", "Ceramic mug"),
        },
    )


@pytest.fixture(name="make_price_list")
def price_list_factory_fixture():
    """Build complete pricing fixtures for existing amount and input-bound tests."""

    default_store = Store("store-a", Currency.EUR)

    def build(
        store=default_store,
        version="prices-v1",
        prices=None,
        **overrides,
    ):
        products = (
            {key: Product(key, "Fixture product") for key in prices}
            if hasattr(prices, "keys")
            else {}
        )
        catalog = Catalog("products-v1", products)
        return PriceList(
            store=store,
            version=version,
            prices=prices,
            catalog=catalog,
            **overrides,
        )

    return build
