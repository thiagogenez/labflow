"""CloudEvents in structured JSON mode, read the way consumers must read them:
only the attributes a consumer needs are checked, and unknown fields are
ignored."""

import json
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from opentelemetry import propagate
from opentelemetry.context import Context

from labflow.rabbitmq import Reject


@dataclass(frozen=True)
class Event:
    """The envelope attributes a consumer uses, and the event's data."""

    id: str
    type: str
    time: datetime
    subject: str
    traceparent: str
    data: dict[str, Any]

    def context(self) -> Context:
        """The trace context the event carries, so its processing becomes a
        child of the span that produced it."""
        return propagate.extract({"traceparent": self.traceparent})


def parse(body: bytes) -> Event:
    """Reads a CloudEvent, raising Reject when it is not one: such a message can
    never be processed."""
    try:
        envelope = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise Reject(f"not JSON: {e}") from None
    if not isinstance(envelope, dict):
        raise Reject("not a JSON object")
    attributes = {}
    for name in ("id", "type", "time"):
        value = envelope.get(name)
        if not isinstance(value, str) or not value:
            raise Reject(f"no {name} attribute")
        attributes[name] = value
    try:
        time = datetime.fromisoformat(attributes["time"])
    except ValueError:
        raise Reject("time is not an RFC 3339 timestamp") from None
    if time.tzinfo is None:
        raise Reject("time has no offset")
    data = envelope.get("data")
    if not isinstance(data, dict):
        raise Reject("data is not a JSON object")
    subject, traceparent = envelope.get("subject"), envelope.get("traceparent")
    return Event(
        id=attributes["id"],
        type=attributes["type"],
        time=time,
        subject=subject if isinstance(subject, str) else "",
        traceparent=traceparent if isinstance(traceparent, str) else "",
        data=data,
    )
