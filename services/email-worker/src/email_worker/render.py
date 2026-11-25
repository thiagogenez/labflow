"""The emails, rendered from Jinja2 templates in text and HTML (D69). Event
fields are untrusted input: the HTML autoescapes them, and headers are built
by the email library, which refuses line breaks."""

import re
from datetime import datetime
from email.headerregistry import Address
from email.message import EmailMessage
from email.policy import SMTP
from email.utils import format_datetime
from typing import Any

from jinja2 import Environment, PackageLoader, StrictUndefined, select_autoescape

_DOMAIN = "labflow.example"
SENDER = Address("LabFlow", "orders", _DOMAIN)

_templates = Environment(
    loader=PackageLoader("email_worker"),
    autoescape=select_autoescape(enabled_extensions=("html",), default=False),
    undefined=StrictUndefined,
    keep_trailing_newline=True,
)


def euros(cents: int) -> str:
    """Formats an amount in cents, such as 123456 as €1,234.56."""
    return f"€{cents // 100:,}.{cents % 100:02d}"


_templates.filters["euros"] = euros


# Text from an event must hold no control characters (C0, DEL, C1).
_CONTROL = re.compile(r"[\x00-\x1f\x7f-\x9f]")
# An order line's quantity, as the event contract bounds it (D60).
_QUANTITY = range(1, 100)


def text(value: Any) -> str:
    """Non-empty text without control characters, or ValueError."""
    if not isinstance(value, str) or not value or _CONTROL.search(value):
        raise ValueError("not usable text")
    return value


def _whole(value: Any, allowed: range | None = None) -> int:
    """An integer, in allowed if given, else not negative: a boolean, a
    fraction or a string is refused, never rounded or converted."""
    if isinstance(value, bool) or not isinstance(value, int):
        raise TypeError("not a whole number")
    if value < 0 or (allowed is not None and value not in allowed):
        raise ValueError("out of range")
    return value


def order_confirmation(
    event_id: str, now: datetime, data: dict[str, Any], link: str
) -> EmailMessage:
    """The confirmation email for an order.placed event. Raises KeyError,
    TypeError, ValueError or email.errors.MessageError when the data lacks
    what the email needs."""
    lines = data["lines"]
    if not isinstance(lines, list) or not lines:
        raise ValueError("no order lines")
    context = {
        "first_name": text(data["customer"]["first_name"]),
        "lines": [
            {
                "name": text(line["name"]),
                "quantity": _whole(line["quantity"], _QUANTITY),
                "gross_amount": _whole(line["gross_amount"]),
            }
            for line in lines
        ],
        "shipping": _whole(data["shipping"]["gross_amount"]),
        "total": _whole(data["totals"]["gross_amount"]),
        "link": link,
    }
    message = EmailMessage(policy=SMTP)
    message["From"] = SENDER
    message["To"] = Address(addr_spec=text(data["customer"]["email"]))
    message["Subject"] = "Your LabFlow order is confirmed"
    message["Date"] = format_datetime(now)
    # Derived from the event ID, so a resend repeats it (D69).
    message["Message-ID"] = f"<{event_id}@{_DOMAIN}>"
    message.set_content(_templates.get_template("order_placed.txt").render(context))
    message.add_alternative(
        _templates.get_template("order_placed.html").render(context), subtype="html"
    )
    return message
