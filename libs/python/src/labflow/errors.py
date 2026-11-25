"""Catching every exception in the few places that must, and logging one
without its message, which may repeat data such as a database row."""

import traceback
from collections.abc import Callable
from typing import Any


def capture(fn: Callable[..., object], *args: Any, **kwargs: Any) -> Exception | None:
    """Calls fn and returns what it raised, if anything: the one place that
    catches every exception, for a handler's bugs, broken connections and a
    background loop that must outlive its errors."""
    try:
        fn(*args, **kwargs)
    except Exception as e:  # noqa: BLE001  # pylint: disable=broad-exception-caught
        return e
    return None


def error_fields(error: BaseException) -> dict[str, Any]:
    """An exception's type and where it was raised, without its message; for
    an AMQP error, the broker's numeric reply code, which names the cause
    (404 NOT_FOUND, 403 ACCESS_REFUSED, 320 CONNECTION_FORCED), while its
    reply text is free-form and could carry anything."""
    fields: dict[str, Any] = {
        "error_type": type(error).__name__,
        "error_frames": [
            f"{frame.f_code.co_filename}:{line} in {frame.f_code.co_name}"
            for frame, line in traceback.walk_tb(error.__traceback__)
        ],
    }
    if isinstance(code := getattr(error, "reply_code", None), int):
        fields["reply_code"] = code
    return fields
