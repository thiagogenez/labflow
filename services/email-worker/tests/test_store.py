"""The send claims (D69) and contacts (D64) against a real PostgreSQL."""

import socket
import threading
import uuid
from datetime import UTC, datetime, timedelta

import psycopg
import pytest
from labflow import postgres
from labflow.rabbitmq import Outage

from email_worker.store import Busy, Claimed, Settled, Store

ORDER = "01a1f874-3be8-7fa4-95f5-868d7d69c709"
TYPE = "labflow.order.placed.v1"


def new_event() -> str:
    """A fresh event ID."""
    return str(uuid.uuid4())


def expire(conninfo: str, event_id: str) -> None:
    """Moves an event's claim into the past, as if its worker had paused."""
    with psycopg.connect(conninfo, autocommit=True) as conn:
        conn.execute(
            "UPDATE sends SET claim_expires_at = now() - interval '1 second'"
            " WHERE event_id = %s",
            (event_id,),
        )


def row(conninfo: str, query: str, *params):
    """One row of a query."""
    with psycopg.connect(conninfo, autocommit=True) as conn:
        return conn.execute(query, params).fetchone()


def test_one_worker_owns_a_send(store, conninfo):
    """A live claim keeps others waiting; an expired one is taken over and
    counted, and only the owner of the claim records the outcome."""
    event, first, second = new_event(), uuid.uuid4(), uuid.uuid4()
    now = datetime.now(UTC)
    assert store.claim(event, TYPE, ORDER, now, first) == Claimed(1)
    claim = store.claim(event, TYPE, ORDER, now, second)
    assert isinstance(claim, Busy) and claim.remaining > timedelta(minutes=1)
    expire(conninfo, event)
    assert store.claim(event, TYPE, ORDER, now, second) == Claimed(2)
    assert not store.sent(event, first)
    assert store.sent(event, second)
    assert store.claim(event, TYPE, ORDER, now, uuid.uuid4()) == Settled("sent")


def test_a_refusal_marks_the_contact(store, conninfo):
    """A refused email records its reply code and makes the order's contact
    undeliverable."""
    event, claim = new_event(), uuid.uuid4()
    now = datetime.now(UTC)
    store.save_contact(ORDER, "ada@example.com", "Ada", now)
    store.claim(event, TYPE, ORDER, now, claim)
    assert not store.undeliverable(ORDER)
    assert not store.refused(event, uuid.uuid4(), ORDER, 550, True)
    assert store.refused(event, claim, ORDER, 550, True)
    assert row(conninfo, "SELECT state, reply_code FROM sends") == ("failed", 550)
    assert store.undeliverable(ORDER)
    assert not store.undeliverable(str(uuid.uuid4()))
    assert store.claim(event, TYPE, ORDER, now, uuid.uuid4()) == Settled("failed")


def test_a_released_claim_is_no_resend(store):
    """An email certainly not sent gives up its claim, so the next attempt is
    a first one, not a resend."""
    event, claim = new_event(), uuid.uuid4()
    now = datetime.now(UTC)
    store.claim(event, TYPE, ORDER, now, claim)
    store.release(event, claim)
    assert store.claim(event, TYPE, ORDER, now, uuid.uuid4()) == Claimed(1)


def test_a_contact_is_kept_60_days_after_payment(store, conninfo):
    """The contact's deletion date is 60 days after the event, and a
    redelivery does not change it."""
    paid = datetime(2026, 11, 20, tzinfo=UTC)
    store.save_contact(ORDER, "ada@example.com", "Ada", paid)
    store.save_contact(ORDER, "other@example.com", "Other", paid + timedelta(days=1))
    assert row(conninfo, "SELECT email, delete_after FROM contacts") == (
        "ada@example.com",
        paid + timedelta(days=60),
    )


def test_the_sweep_deletes_only_what_expired(store, conninfo):
    """Sends go 30 days after their event, contacts after their date."""
    now = datetime.now(UTC)
    old, recent = new_event(), new_event()
    store.claim(old, TYPE, ORDER, now - timedelta(days=31), uuid.uuid4())
    store.claim(recent, TYPE, ORDER, now - timedelta(days=29), uuid.uuid4())
    store.save_contact(ORDER, "ada@example.com", "Ada", now - timedelta(days=61))
    kept = str(uuid.uuid4())
    store.save_contact(kept, "bob@example.com", "Bob", now - timedelta(days=59))
    assert store.sweep() == {"sends": 1, "contacts": 1}
    assert row(conninfo, "SELECT array_agg(event_id::text) FROM sends") == ([recent],)
    assert row(conninfo, "SELECT array_agg(order_id::text) FROM contacts") == ([kept],)


def test_a_frozen_database_is_an_outage_within_the_deadline(
    store, postgres_server, monkeypatch
):
    """A database that stops answering after the connection opened fails the
    call at the client's deadline, as an outage, instead of holding the
    message and the shutdown forever."""
    monkeypatch.setattr(postgres, "CALL_DEADLINE_SECONDS", 2)
    store.undeliverable(ORDER)  # the connection is open
    outcome = []

    def call() -> None:
        try:
            store.undeliverable(ORDER)
        except Outage as e:
            outcome.append(e)

    caller = threading.Thread(target=call, daemon=True)
    postgres_server.pause()
    try:
        caller.start()
        caller.join(8)
    finally:
        postgres_server.unpause()
    assert not caller.is_alive(), "the call outlived its deadline"
    assert outcome, "the call raised no outage"
    assert not store.undeliverable(ORDER)  # a new connection works


def test_a_read_only_database_is_an_outage(store, conninfo):
    """A primary demoted during a switchover answers read-only: an outage
    that holds the message, named by its SQLSTATE, not a bug that spends its
    delivery attempts."""
    with psycopg.connect(conninfo, autocommit=True) as conn:
        dbname = conn.info.dbname
        conn.execute(
            f'ALTER DATABASE "{dbname}" SET default_transaction_read_only = on'
        )
    with pytest.raises(Outage, match="ReadOnlySqlTransaction 25006"):
        store.save_contact(ORDER, "ada@example.com", "Ada", datetime.now(UTC))


def test_an_unreachable_database_is_an_outage():
    """No database answering is an outage, which holds the message."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    store = Store(f"host=127.0.0.1 port={port} user=x password=y dbname=z")
    with pytest.raises(Outage, match="database"):
        store.claim(new_event(), TYPE, ORDER, datetime.now(UTC), uuid.uuid4())
