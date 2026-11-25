"""The handler end to end, with a real database and Mailpit: one email per
event, refusals, outages, age, and what reaches logs and traces (D64, D69)."""

import json
import logging
import time
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

import psycopg
import pytest
from labflow.rabbitmq import Delivery, Outage, Reject, Retry
from opentelemetry.trace import StatusCode
from prometheus_client import REGISTRY

from email_worker.handler import Handler
from email_worker.links import Links, token
from email_worker.mail import Mailer, MaybeSent, NotSent
from email_worker.store import Store

PAGE = "https://shop.example/orders/"
EMAILS_TOTAL = "email_worker_emails_total"
RESENDS_TOTAL = "email_worker_resend_attempts_total"


@pytest.fixture(name="event")
def fixture_event(example):
    """Builds an order.placed event of a given age with fresh IDs, or for a
    given order, with changes to its data: returns both IDs and the body."""

    def build(age=timedelta(seconds=1), order_id=None, data=None, **envelope_change):
        envelope = json.loads(json.dumps(example))
        event_id, order_id = str(uuid.uuid4()), order_id or str(uuid.uuid4())
        envelope |= {
            "id": event_id,
            "subject": order_id,
            "time": (datetime.now(UTC) - age).isoformat(),
        }
        envelope["data"]["order_id"] = order_id
        for path, value in (data or {}).items():
            *parents, key = path.split(".")
            target = envelope["data"]
            for parent in parents:
                target = target[int(parent) if isinstance(target, list) else parent]
            target[int(key) if isinstance(target, list) else key] = value
        envelope |= envelope_change
        return event_id, order_id, json.dumps(envelope).encode()

    return build


def count(name: str, **labels: str) -> float:
    """A counter's current value."""
    return REGISTRY.get_sample_value(name, labels) or 0.0


def sends(conninfo: str) -> list[tuple]:
    """Every send record's event ID, state, attempts and reply code."""
    with psycopg.connect(conninfo) as conn:
        return conn.execute(
            "SELECT event_id::text, state, attempts, reply_code FROM sends ORDER BY updated_at"
        ).fetchall()


def fails_once(store, method: str):
    """Makes the store's first call of method find the database down."""
    real, failed = getattr(store, method), []

    def flaky(*args):
        if not failed:
            failed.append(True)
            raise Outage("database: OperationalError")
        return real(*args)

    setattr(store, method, flaky)
    return store


@dataclass
class Counting:
    """A mailer that counts its sends, failing the first, or every one with
    always, as a lost reply would when maybe_sent is set."""

    mailer: Mailer
    maybe_sent: bool = False
    calls: int = 0
    always: bool = False

    def send(self, message) -> None:
        """Sends through the real mailer."""
        self.calls += 1
        self.mailer.send(message)
        if self.maybe_sent and (self.calls == 1 or self.always):
            raise MaybeSent("no answer to the email: TimeoutError")


@dataclass
class World:
    """What a handler test works with: a fresh database, the inbox, a counted
    mailer and an event builder."""

    store: Store
    mailer: Mailer
    mail: Counting
    inbox: Any
    conninfo: str
    event: Any
    token_key: bytes

    @property
    def links(self) -> Links:
        """Links to the test's order page."""
        return Links(PAGE, self.token_key)

    def handler(self, store: Store | None = None, mail: Any = None) -> Handler:
        """The handler, with the real store and the counted mailer unless
        given others."""
        return Handler(
            store or self.store, mail or self.mail, self.links, logging.getLogger("t")
        )

    def sends(self) -> list[tuple]:
        """Every send record's event ID, state, attempts and reply code."""
        return sends(self.conninfo)


@pytest.fixture(name="w")
def fixture_world(request):
    """The test's world, on a fresh database."""
    get = request.getfixturevalue
    mailer = get("mailer")
    return World(
        get("store"),
        mailer,
        Counting(mailer),
        get("inbox"),
        get("conninfo"),
        get("event"),
        get("token_key"),
    )


def test_sends_one_email_per_event(w):
    """The email arrives once with its link; a redelivery is skipped."""
    handler = w.handler()
    event_id, order_id, body = w.event()
    handler(Delivery(body))
    handler(Delivery(body))
    [received] = w.inbox.messages()
    assert received["MessageID"] == f"{event_id}@labflow.example"
    link = f"{PAGE}{order_id}#token={token(w.token_key, order_id)}"
    assert link in w.inbox.text(received)
    assert w.sends() == [(event_id, "sent", 1, None)]


def test_a_refused_email_is_settled_and_its_contact_gets_no_more(w):
    """A 5xx reply records the refusal and returns normally, so the message is
    acknowledged; a later event for the same order sends nothing."""
    handler = w.handler()
    w.inbox.chaos(recipient=550)
    refused = count(EMAILS_TOTAL, outcome="refused")
    undeliverable = count(EMAILS_TOTAL, outcome="undeliverable")
    event_id, order_id, body = w.event()
    handler(Delivery(body))
    assert w.sends() == [(event_id, "failed", 1, 550)]
    assert count(EMAILS_TOTAL, outcome="refused") == refused + 1
    w.inbox.chaos()
    handler(Delivery(w.event(order_id=order_id)[2]))
    assert count(EMAILS_TOTAL, outcome="undeliverable") == undeliverable + 1
    assert w.mail.calls == 1 and not w.inbox.messages()


def test_a_refusal_is_recorded_on_retry_without_a_second_send(w):
    """If the database fails while recording a refusal, the retry records it
    and does not ask the mail server again."""
    handler = w.handler(store=fails_once(w.store, "refused"))
    w.inbox.chaos(recipient=550)
    event_id, _, body = w.event()
    held = Delivery(body)
    with pytest.raises(Outage):
        handler(held)
    w.inbox.chaos()
    handler(held)
    assert w.mail.calls == 1 and not w.inbox.messages()
    assert w.sends() == [(event_id, "failed", 1, 550)]


def test_an_smtp_outage_holds_the_email_and_counts_no_resend(w):
    """While the mail server refuses the login, the handler raises an outage
    and gives its claim back; once it answers, the same delivery sends the
    email as a first attempt."""
    handler = w.handler()
    w.inbox.chaos(authentication=454)
    event_id, _, body = w.event()
    held = Delivery(body)
    with pytest.raises(NotSent):
        handler(held)
    assert not w.sends()
    w.inbox.chaos()
    resends = count(RESENDS_TOTAL)
    handler(held)
    assert w.sends() == [(event_id, "sent", 1, None)]
    assert count(RESENDS_TOTAL) == resends


def test_a_claim_not_given_back_is_given_back_first(w):
    """If the database fails while giving a claim back after an outage, the
    next attempt gives it back first, so the send is not counted as a
    resend."""
    handler = w.handler(store=fails_once(w.store, "release"))
    w.inbox.chaos(authentication=454)
    event_id, _, body = w.event()
    held = Delivery(body)
    with pytest.raises(NotSent):
        handler(held)
    assert [row[1] for row in w.sends()] == ["sending"]
    w.inbox.chaos()
    resends = count(RESENDS_TOTAL)
    handler(held)
    assert w.sends() == [(event_id, "sent", 1, None)]
    assert count(RESENDS_TOTAL) == resends


def test_a_possibly_sent_email_is_resent_only_after_its_claim_expires(w):
    """After a lost reply the claim stays, so a retry waits; once the claim
    expires, the retry takes it over and resends with the same Message-ID,
    counted."""
    handler = w.handler(mail=Counting(w.mailer, maybe_sent=True))
    event_id, _, body = w.event()
    held = Delivery(body)
    with pytest.raises(MaybeSent):
        handler(held)
    with pytest.raises(Retry):
        handler(held)
    with psycopg.connect(w.conninfo, autocommit=True) as conn:
        conn.execute("UPDATE sends SET claim_expires_at = now() - interval '1 second'")
    resends = count(RESENDS_TOTAL)
    handler(held)
    assert count(RESENDS_TOTAL) == resends + 1
    assert w.sends() == [(event_id, "sent", 2, None)]
    ids = [m["MessageID"] for m in w.inbox.messages()]
    assert ids == [f"{event_id}@labflow.example"] * 2


def test_an_accepted_email_is_recorded_on_retry_even_past_7_days(w):
    """If the database fails after the mail server accepted the email, the
    retry records it without sending again, even once the event has passed
    its 7 days."""
    handler = w.handler(store=fails_once(w.store, "sent"))
    event_id, _, body = w.event(age=timedelta(days=7) - timedelta(seconds=2))
    held = Delivery(body)
    with pytest.raises(Outage):
        handler(held)
    time.sleep(3)
    handler(held)
    assert w.mail.calls == 1 and len(w.inbox.messages()) == 1
    assert w.sends() == [(event_id, "sent", 1, None)]


def test_an_event_older_than_7_days_is_skipped(w):
    """Processing ends 7 days after the event's time (D64)."""
    before = count(EMAILS_TOTAL, outcome="expired")
    w.handler()(Delivery(w.event(age=timedelta(days=7, minutes=1))[2]))
    assert count(EMAILS_TOTAL, outcome="expired") == before + 1
    assert not w.inbox.messages() and not w.sends()


@pytest.mark.parametrize(
    "envelope, data",
    [
        ({"type": "labflow.order.shipped.v1"}, {}),
        ({"id": "not-a-uuid"}, {}),
        ({}, {"order_id": 12345}),
        ({}, {"customer.email": "ada@"}),
        ({}, {"customer.email": "ada@example.com\r\nBcc: eve@example.com"}),
        ({}, {"customer.first_name": "Ada\nBcc: eve@example.com"}),
        ({}, {"customer.first_name": None}),
        ({}, {"customer.first_name": ""}),
        ({}, {"lines": None}),
        ({}, {"lines": []}),
        ({}, {"lines": {}}),
        ({}, {"lines": ""}),
        ({}, {"lines.0.quantity": 100}),
        ({}, {"lines.0.name": None}),
        ({}, {"lines.0.quantity": True}),
        ({}, {"lines.0.quantity": 0}),
        ({}, {"lines.0.gross_amount": 89.5}),
        ({}, {"totals.gross_amount": "18140"}),
    ],
)
def test_an_event_without_what_its_email_needs_is_rejected(w, envelope, data):
    """A wrong type, an ID that is no UUID, or data the email cannot use goes
    to the dead-letter queue at once, before the database or the mail
    server."""
    with pytest.raises(Reject):
        w.handler()(Delivery(w.event(data=data, **envelope)[2]))
    assert w.mail.calls == 0 and not w.sends()


def test_errors_reach_logs_and_traces_without_their_messages(w, spans, caplog):
    """An unexpected error's message may repeat customer data, such as a
    database row; the span records only its type."""

    def broken(*_):
        raise RuntimeError("Failing row contains (ada@example.com, Ada)")

    w.store.save_contact = broken
    with caplog.at_level(logging.DEBUG), pytest.raises(RuntimeError):
        w.handler()(Delivery(w.event()[2]))
    [span] = spans.get_finished_spans()
    assert span.status.status_code == StatusCode.ERROR
    assert span.status.description == "RuntimeError"
    assert not span.events
    assert "ada@example.com" not in caplog.text


@pytest.mark.parametrize("stop_at", ["start", "save_contact", "undeliverable", "claim"])
def test_a_stopping_worker_returns_the_message_before_each_long_step(w, stop_at):
    """Once the worker is stopping, the message is returned, uncounted, before
    the next database call or the send, so the email in hand finishes within
    the grace period; a claim already taken is given back, so nothing counts
    as a resend."""
    _, _, body = w.event()
    delivery, calls = Delivery(body), []
    for name in ("save_contact", "undeliverable", "claim"):
        real = getattr(w.store, name)

        def call(*args, name=name, real=real):
            calls.append(name)
            result = real(*args)
            if name == stop_at:
                delivery.stopping.set()
            return result

        setattr(w.store, name, call)
    if stop_at == "start":
        delivery.stopping.set()
    with pytest.raises(Retry, match="stopping"):
        w.handler()(delivery)
    steps = ["save_contact", "undeliverable", "claim"]
    done = steps[: steps.index(stop_at) + 1] if stop_at != "start" else []
    assert calls == done, "a database call ran after the stop"
    assert w.mail.calls == 0 and not w.sends()


def test_a_refused_sender_does_not_mark_the_contact(w):
    """A refused sender or message is no fault of the address: the send is
    recorded as failed, but a later email for the order still goes out."""
    handler = w.handler()
    w.inbox.chaos(sender=550)
    event_id, order_id, body = w.event()
    handler(Delivery(body))
    assert w.sends() == [(event_id, "failed", 1, 550)]
    w.inbox.chaos()
    handler(Delivery(w.event(order_id=order_id)[2]))
    assert len(w.inbox.messages()) == 1, "a refused sender marked the contact"


def test_a_claim_owed_back_is_given_back_even_past_7_days(w):
    """A claim that could not be given back is given back on the next attempt
    even when the event has passed its 7 days meanwhile."""
    handler = w.handler(store=fails_once(w.store, "release"))
    w.inbox.chaos(authentication=454)
    _, _, body = w.event(age=timedelta(days=7) - timedelta(seconds=2))
    held = Delivery(body)
    with pytest.raises(NotSent):
        handler(held)
    w.inbox.chaos()
    time.sleep(3)
    handler(held)
    assert not w.sends() and not w.inbox.messages()


def test_a_resend_that_gets_no_answer_again_is_recorded_as_sent(w):
    """A server that accepts but answers too slowly would otherwise get the
    email again every 2 minutes; the takeover that again gets no answer is
    recorded as sent, so it gets two copies at most."""
    handler = w.handler(mail=Counting(w.mailer, maybe_sent=True))
    handler.mailer.always = True
    event_id, _, body = w.event()
    held = Delivery(body)
    with pytest.raises(MaybeSent):
        handler(held)
    with psycopg.connect(w.conninfo, autocommit=True) as conn:
        conn.execute("UPDATE sends SET claim_expires_at = now() - interval '1 second'")
    handler(Delivery(body))
    assert w.sends() == [(event_id, "sent", 2, None)], "the resend was not recorded"
    handler(Delivery(body))
    assert len(w.inbox.messages()) == 2
