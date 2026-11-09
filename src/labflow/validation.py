"""Shared field validation; callers own the domain-specific limits."""


def validate_integer_range(
    value: int,
    name: str,
    *,
    minimum: int,
    maximum: int,
    range_message: str | None = None,
) -> None:
    """Require an integer within inclusive bounds, excluding booleans.

    Args:
        value: Field value to validate without coercion.
        name: Field name used in error messages.
        minimum: Inclusive lower bound supplied by the owning domain.
        maximum: Inclusive upper bound supplied by the owning domain.
        range_message: Optional domain-specific message for an invalid range.
            Defaults to a message containing the field name and bounds.

    Raises:
        TypeError: Value is not an integer, or is a boolean.
        ValueError: Value is outside the supplied bounds.
    """
    if isinstance(value, bool) or not isinstance(value, int):
        raise TypeError(f"{name} must be an integer")
    if not minimum <= value <= maximum:
        raise ValueError(
            range_message
            if range_message is not None
            else f"{name} must be between {minimum} and {maximum}"
        )
