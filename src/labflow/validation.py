"""Shared field validation and normalization; callers own domain-specific rules."""

from datetime import UTC, datetime


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


def normalize_utc_datetime(value: datetime, name: str) -> datetime:
    """Validate an offset-aware datetime and normalize it to UTC.

    Args:
        value: Timestamp with an explicit UTC offset. Strings are not parsed
            and naive timestamps are not assigned an implicit timezone.
        name: Field name used in validation error messages.

    Returns:
        An immutable datetime representing the same instant in UTC.

    Raises:
        TypeError: Value is not a datetime.
        ValueError: Value does not specify a UTC offset.
    """
    if not isinstance(value, datetime):
        raise TypeError(f"{name} must be a datetime")
    if value.utcoffset() is None:
        raise ValueError(f"{name} must be offset-aware")
    return value.astimezone(UTC)
