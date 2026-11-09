"""Explicit currency codes supported by the pricing domain."""

from enum import StrEnum

MAX_AMOUNT_CENTS = 2**63 - 1


class Currency(StrEnum):
    """Supported currencies, represented by their code at serialization boundaries.

    Use Currency.EUR or Currency.BRL in domain objects; both use integer cents
    with two decimal places. Parse external codes with Currency(value), which
    raises ValueError for unsupported strings. Additional currencies require
    defining their amount precision policy.
    This type does not define exchange rates, conversion or rounding behavior.
    """

    EUR = "EUR"
    BRL = "BRL"
