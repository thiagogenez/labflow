"""Shared product metadata, independent of stores, prices and inventory."""

from collections.abc import Mapping
from dataclasses import dataclass
from types import MappingProxyType


@dataclass(frozen=True, slots=True)
class Product:
    """Metadata for one independently identifiable product or variant.

    Args:
        product_id: Nonempty SKU without surrounding whitespace.
        name: Nonempty display name without surrounding whitespace.
        description: Optional descriptive text; defaults to an empty string.

    Raises:
        TypeError: A field is not a string.
        ValueError: The identifier or name is empty or padded with whitespace.
    """

    product_id: str
    name: str
    description: str = ""

    def __post_init__(self) -> None:
        """Validate identifiers and immutable text metadata."""
        validate_identifier(self.product_id, "product_id")
        validate_identifier(self.name, "name")
        if not isinstance(self.description, str):
            raise TypeError("description must be a string")


@dataclass(frozen=True, slots=True)
class Catalog:
    """An immutable product snapshot that multiple store price lists can share.

    Args:
        version: Nonempty product snapshot identifier without surrounding
            whitespace. It is separate from each store's price-list version.
        products: Mapping from SKU strings to immutable Product objects. Each key
            must match its product's identifier. Empty catalogs are allowed.
            The mapping must be stable during construction; it is copied and
            exposed through a read-only view. No price, currency, store or stock
            quantities are recorded here.

    Raises:
        TypeError: Version, mapping, keys or product objects have invalid types.
        ValueError: An identifier is invalid or a key does not match its product.

    Version uniqueness will require persistence; this object is not a registry.
    """

    version: str
    products: Mapping[str, Product]

    def __post_init__(self) -> None:
        """Copy and validate product membership before exposing the snapshot."""
        validate_identifier(self.version, "version")
        if not isinstance(self.products, Mapping):
            raise TypeError("products must be a mapping")
        products = dict(self.products)
        for product_id, product in products.items():
            validate_identifier(product_id, "product_id")
            if not isinstance(product, Product):
                raise TypeError("products must contain Product instances")
            if product_id != product.product_id:
                raise ValueError("catalog key must match product_id")
        object.__setattr__(self, "products", MappingProxyType(products))


def validate_identifier(value: str, name: str) -> None:
    """Require an identifier string without empty or surrounding whitespace.

    Args:
        value: Catalog version or product identifier to validate.
        name: Field name included in validation error messages.

    Raises:
        TypeError: The value is not a string.
        ValueError: The value is empty or has surrounding whitespace.
    """
    if not isinstance(value, str):
        raise TypeError(f"{name} must be a string")
    if not value or value != value.strip():
        raise ValueError(f"{name} must be nonempty without surrounding whitespace")
