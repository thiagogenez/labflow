"""Shared product metadata snapshots and their immutable boundaries."""

from dataclasses import FrozenInstanceError

import pytest

from labflow.catalog import Catalog, Product
from labflow.price_list import PriceList


def test_product_metadata_is_shared_between_store_price_lists(
    shared_catalog, berlin_store, saopaulo_store
):
    """Two stores reuse the exact same metadata while choosing independent prices."""
    berlin = PriceList(berlin_store, "prices-v1", {"sku-shirt": 1990}, shared_catalog)
    saopaulo = PriceList(
        saopaulo_store, "prices-v1", {"sku-shirt": 5990}, shared_catalog
    )

    assert berlin.catalog is saopaulo.catalog is shared_catalog
    assert berlin.catalog.products["sku-shirt"].name == "Black T-shirt, size M"
    assert berlin.prices["sku-shirt"] == 1990
    assert saopaulo.prices["sku-shirt"] == 5990


def test_catalog_copies_source_products():
    """Later additions and replacements cannot alter a published snapshot."""
    product = Product("shirt", "Shirt")
    source = {"shirt": product}
    catalog = Catalog("products-v1", source)
    source["shirt"] = Product("shirt", "Updated shirt")
    source["mug"] = Product("mug", "Mug")
    assert dict(catalog.products) == {"shirt": product}


def test_catalog_products_are_read_only(shared_catalog):
    """Metadata cannot be replaced through the catalog's public mapping."""
    with pytest.raises(TypeError):
        shared_catalog.products["sku-shirt"] = Product("sku-shirt", "Changed")


@pytest.mark.parametrize("field", ["version", "products"])
def test_catalog_fields_are_frozen(shared_catalog, field):
    """Prevent replacing catalog identity or membership."""
    with pytest.raises(FrozenInstanceError):
        setattr(shared_catalog, field, "changed")


@pytest.mark.parametrize("field", ["product_id", "name", "description"])
def test_product_fields_are_frozen(field):
    """Every metadata value referenced by a catalog remains immutable."""
    product = Product("shirt", "Shirt", "Cotton")
    with pytest.raises(FrozenInstanceError):
        setattr(product, field, "changed")


@pytest.mark.parametrize("field", ["product_id", "name"])
@pytest.mark.parametrize("value", ["", " ", " padded "])
def test_rejects_invalid_product_identifiers_and_names(field, value):
    """Reject empty or padded required metadata."""
    arguments = {"product_id": "shirt", "name": "Shirt"}
    arguments[field] = value
    with pytest.raises(ValueError, match=field):
        Product(**arguments)


@pytest.mark.parametrize("field", ["product_id", "name", "description"])
def test_rejects_non_string_product_metadata(field):
    """All metadata fields have predictable immutable string values."""
    arguments = {"product_id": "shirt", "name": "Shirt", "description": "Cotton"}
    arguments[field] = None
    with pytest.raises(TypeError, match=field):
        Product(**arguments)


@pytest.mark.parametrize("version", ["", " ", " padded "])
def test_rejects_invalid_catalog_version(version):
    """Versions are explicit identifiers for product snapshots."""
    with pytest.raises(ValueError, match="version"):
        Catalog(version, {})


def test_rejects_non_string_catalog_version():
    """Validate catalog version types at construction."""
    with pytest.raises(TypeError, match="version"):
        Catalog(None, {})


@pytest.mark.parametrize("products", [None, [], "shirt"])
def test_rejects_non_mapping_products(products):
    """Catalog membership must be a SKU-to-product mapping."""
    with pytest.raises(TypeError, match="products must be a mapping"):
        Catalog("products-v1", products)


def test_rejects_product_key_mismatch():
    """A product cannot be looked up under another SKU."""
    with pytest.raises(ValueError, match="key must match product_id"):
        Catalog("products-v1", {"other": Product("shirt", "Shirt")})


def test_rejects_non_product_members():
    """Price-like primitive values are not product metadata."""
    with pytest.raises(TypeError, match="Product instances"):
        Catalog("products-v1", {"shirt": 1990})


def test_empty_catalog_is_valid():
    """An empty product snapshot offers no product definitions."""
    assert not Catalog("products-v1", {}).products


def test_price_list_rejects_product_absent_from_catalog(berlin_store, shared_catalog):
    """A store can price only products that exist in the shared snapshot."""
    with pytest.raises(ValueError, match="absent from catalog"):
        PriceList(berlin_store, "prices-v1", {"unknown": 100}, shared_catalog)
