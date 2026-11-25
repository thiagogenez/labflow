"""The confirmation email: content, headers, and event fields as untrusted
input."""

import json
from datetime import UTC, datetime
from pathlib import Path

import pytest

from email_worker import render

EXAMPLE = (
    Path(__file__).resolve().parents[3]
    / "contracts/events/examples/labflow.order.placed.v1.json"
)
NOW = datetime(2026, 11, 20, 10, 16, tzinfo=UTC)
LINK = "https://shop.example/orders/o1#token=t"


def data(**customer: str) -> dict:
    """The example event's data, with customer fields replaced."""
    d = json.loads(EXAMPLE.read_bytes())["data"]
    d["customer"] |= customer
    return d


def parts(message) -> tuple[str, str]:
    """The text and HTML alternatives."""
    text = message.get_body(("plain",)).get_content()
    html = message.get_body(("html",)).get_content()
    return text, html


def test_confirmation():
    """Headers, amounts and link, in both alternatives."""
    message = render.order_confirmation("e1", NOW, data(), LINK)
    assert message["From"] == "LabFlow <orders@labflow.example>"
    assert message["To"] == "ada@example.com"
    assert message["Subject"] == "Your LabFlow order is confirmed"
    assert message["Message-ID"] == "<e1@labflow.example>"
    assert message["Date"] == "Fri, 20 Nov 2026 10:16:00 +0000"
    text, html = parts(message)
    for part in (text, html):
        assert "Hello Ada," in part
        assert "Adjustable pipette, 10 to 100 µL" in part
        assert "€89.00" in part and "€37.50" in part and "€5.90" in part
        assert "€181.40" in part
        assert LINK in part


def test_html_escapes_customer_text():
    """A name that is markup shows as text in HTML and stays as typed in the
    text alternative."""
    text, html = parts(
        render.order_confirmation("e1", NOW, data(first_name="<b>Ada</b>"), LINK)
    )
    assert "&lt;b&gt;Ada&lt;/b&gt;" in html and "<b>Ada</b>" not in html
    assert "Hello <b>Ada</b>," in text


@pytest.mark.parametrize(
    "email",
    [
        "ada@example.com\r\nBcc: eve@example.com",
        "ada@example.com\nX: y",
        "not an address",
    ],
)
def test_headers_refuse_injection(email):
    """An address that would add a header, or is no address, is refused."""
    with pytest.raises(ValueError):
        render.order_confirmation("e1", NOW, data(email=email), LINK)


@pytest.mark.parametrize(
    "cents, want", [(0, "€0.00"), (5, "€0.05"), (123456, "€1,234.56")]
)
def test_euros(cents, want):
    """Amounts in cents as euros."""
    assert render.euros(cents) == want
