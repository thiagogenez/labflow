"""CloudEvents read as consumers must read them."""

import json
from datetime import UTC, datetime
from pathlib import Path

import pytest
from opentelemetry import trace

from labflow import events
from labflow.rabbitmq import Reject

EXAMPLE = (
    Path(__file__).resolve().parents[3]
    / "contracts/events/examples/labflow.order.placed.v1.json"
)


def changed(change) -> bytes:
    """The contract example with change applied to its envelope."""
    envelope = json.loads(EXAMPLE.read_bytes())
    change(envelope)
    return json.dumps(envelope).encode()


def test_parses_the_contract_example():
    """The envelope's attributes, its data and its trace context come through."""
    event = events.parse(EXAMPLE.read_bytes())
    assert event.id == "01a1f874-3e4b-772c-8d7e-d1c457153733"
    assert event.type == "labflow.order.placed.v1"
    assert event.time == datetime(2026, 11, 20, 10, 15, 30, 123000, tzinfo=UTC)
    assert event.subject == event.data["order_id"]
    parent = trace.get_current_span(event.context()).get_span_context()
    assert format(parent.trace_id, "032x") == "4bf92f3577b34da6a3ce929d0e0e4736"


def test_ignores_unknown_fields():
    """Consumers tolerate fields a newer producer adds."""
    body = changed(
        lambda e: e.update(newattribute="x", data=e["data"] | {"new_field": {"n": 1}})
    )
    assert events.parse(body).data["new_field"] == {"n": 1}


@pytest.mark.parametrize(
    "body, reason",
    [
        (b"\xff", "not JSON"),
        (b"{", "not JSON"),
        (b"[]", "not a JSON object"),
        (changed(lambda e: e.pop("id")), "no id"),
        (changed(lambda e: e.update(type=7)), "no type"),
        (changed(lambda e: e.update(time="yesterday")), "time is not an RFC 3339"),
        (changed(lambda e: e.update(time="2026-11-20T10:15:30")), "time has no offset"),
        (changed(lambda e: e.update(data="x")), "data is not a JSON object"),
    ],
)
def test_rejects_what_is_not_a_cloudevent(body, reason):
    """A message without what every consumer needs can never be processed."""
    with pytest.raises(Reject, match=reason):
        events.parse(body)


def test_an_event_without_trace_context_starts_a_trace():
    """No traceparent means no parent span."""
    event = events.parse(changed(lambda e: e.pop("traceparent")))
    assert not trace.get_current_span(event.context()).get_span_context().is_valid
