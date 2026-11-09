"""Server-controlled store configuration used by store price lists."""

from dataclasses import dataclass

from labflow.currency import Currency


@dataclass(frozen=True, slots=True)
class Store:
    """Identify a store and its fixed pricing currency.

    Args:
        store_id: Nonempty store identifier without surrounding whitespace,
            such as "store-berlin".
        currency: Currency.EUR or Currency.BRL, applied to every price list and
            order for this store. Raw strings are rejected.

    Raises:
        TypeError: The identifier is not a string or currency is not a Currency.
        ValueError: The identifier is empty or has surrounding whitespace.

    This immutable configuration is not authorization or a global store registry.
    The server must resolve a canonical configuration for each identifier;
    persistence will enforce store identity and its currency across requests.
    """

    store_id: str
    currency: Currency

    def __post_init__(self) -> None:
        """Validate the store identity and supported currency."""
        validate_store_configuration(self.store_id, self.currency)


def validate_store_configuration(store_id: str, currency: Currency) -> None:
    """Validate shared store fields in configurations and historical quotations.

    Args:
        store_id: Nonempty store identifier without surrounding whitespace.
        currency: Explicit Currency member used by every price in the store.

    Raises:
        TypeError: The identifier is not a string or currency is not a Currency.
        ValueError: The identifier is empty or has surrounding whitespace.
    """
    if not isinstance(store_id, str):
        raise TypeError("store_id must be a string")
    if not store_id or store_id != store_id.strip():
        raise ValueError("store_id must be nonempty without surrounding whitespace")
    if not isinstance(currency, Currency):
        raise TypeError("currency must be a Currency member")
