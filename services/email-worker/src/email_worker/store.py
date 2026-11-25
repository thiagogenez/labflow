"""The worker's database: send claims (D69) and contacts (D64).

Any failure to reach the database, or a statement, lock wait or call running
out of time, raises Outage, so the message waits for the database instead of
counting as a failed delivery."""

import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timedelta

import psycopg
from labflow import postgres
from labflow.rabbitmq import Outage

# Longer than an email's SMTP deadline, so a live sender keeps its claim (D69).
CLAIM = timedelta(minutes=2)
SEND_RETENTION = timedelta(days=30)
CONTACT_RETENTION = timedelta(days=60)
_SWEEP_BATCH = 1000


@dataclass(frozen=True)
class Claimed:
    """This worker owns the send; attempts above 1 mean a takeover."""

    attempts: int


@dataclass(frozen=True)
class Settled:
    """The email was sent or refused already."""

    state: str


@dataclass(frozen=True)
class Busy:
    """Another worker owns a live claim for this long."""

    remaining: timedelta


class Store:
    """One connection, opened on first use and again after a failure."""

    def __init__(self, dsn: str):
        self._dsn = dsn
        self._conn: psycopg.Connection | None = None

    @contextmanager
    def _connection(self) -> Iterator[psycopg.Connection]:
        try:
            if self._conn is None or self._conn.closed:
                self._conn = postgres.connect(self._dsn)
            with postgres.deadline(self._conn):
                yield self._conn
        except (
            psycopg.OperationalError,
            psycopg.InterfaceError,
            # A primary demoted during a switchover answers read-only.
            psycopg.errors.ReadOnlySqlTransaction,
        ) as e:
            self.close()
            # The class and SQLSTATE tell a refused password from a stopped
            # server; neither repeats data.
            raise Outage(
                f"database: {type(e).__name__} {e.sqlstate or ''}".strip()
            ) from None

    def close(self) -> None:
        """Closes the connection, if open."""
        if self._conn is not None:
            self._conn.close()
            self._conn = None

    def save_contact(
        self, order_id: str, email: str, first_name: str, paid_at: datetime
    ) -> None:
        """Keeps the order's contact until 60 days after payment."""
        with self._connection() as conn:
            conn.execute(
                _SAVE_CONTACT,
                (order_id, email, first_name, paid_at + CONTACT_RETENTION),
            )

    def undeliverable(self, order_id: str) -> bool:
        """Whether the mail server refused an email to the order's contact for
        good."""
        with self._connection() as conn:
            row = conn.execute(_UNDELIVERABLE_STATE, (order_id,)).fetchone()
        return bool(row and row[0])

    def claim(
        self,
        event_id: str,
        event_type: str,
        order_id: str,
        event_time: datetime,
        claim_id: uuid.UUID,
    ) -> Claimed | Settled | Busy:
        """Claims the send of an event's email, taking over an expired claim."""
        with self._connection() as conn:
            row = conn.execute(
                _CLAIM,
                (event_id, event_type, order_id, event_time, claim_id, CLAIM),
            ).fetchone()
            if row is not None:
                return Claimed(row[0])
            row = conn.execute(_CLAIM_STATE, (event_id,)).fetchone()
        if row is None:  # swept in between: try again
            return Busy(timedelta(0))
        state, remaining = row
        return Settled(state) if state != "sending" else Busy(remaining)

    def sent(self, event_id: str, claim_id: uuid.UUID) -> bool:
        """Records the email as sent, only while this worker owns the claim."""
        with self._connection() as conn:
            return conn.execute(_SENT, (event_id, claim_id)).rowcount == 1

    def refused(
        self,
        event_id: str,
        claim_id: uuid.UUID,
        order_id: str,
        code: int,
        undeliverable: bool,
    ) -> bool:
        """Records the email as refused for good, and marks the order's
        contact undeliverable if the address was refused, only while this
        worker owns the claim."""
        with self._connection() as conn, conn.transaction():
            if conn.execute(_REFUSED, (code, event_id, claim_id)).rowcount != 1:
                return False
            if undeliverable:
                conn.execute(_UNDELIVERABLE, (order_id,))
            return True

    def release(self, event_id: str, claim_id: uuid.UUID) -> None:
        """Drops a claim whose email was certainly not accepted, so the next
        attempt is not counted as a resend."""
        with self._connection() as conn:
            conn.execute(_RELEASE, (event_id, claim_id))

    def sweep(self) -> dict[str, int]:
        """Deletes sends 30 days after their event and contacts past their
        date, in batches that concurrent sweeps skip past each other in."""
        statements = {
            "sends": (_SWEEP_SENDS, (SEND_RETENTION, _SWEEP_BATCH)),
            "contacts": (_SWEEP_CONTACTS, (_SWEEP_BATCH,)),
        }
        deleted = dict.fromkeys(statements, 0)
        for table, (statement, params) in statements.items():
            while True:
                with self._connection() as conn:  # each batch within the deadline
                    count = conn.execute(statement, params).rowcount
                deleted[table] += count
                if count < _SWEEP_BATCH:
                    break
        return deleted


_SAVE_CONTACT = """
INSERT INTO contacts (order_id, email, first_name, delete_after)
VALUES (%s, %s, %s, %s)
ON CONFLICT (order_id) DO NOTHING
"""

_CLAIM = """
INSERT INTO sends (
    event_id, event_type, order_id, event_time, state, claim_id, claim_expires_at
)
VALUES (%s, %s, %s, %s, 'sending', %s, now() + %s)
ON CONFLICT (event_id) DO UPDATE
SET claim_id = excluded.claim_id,
    claim_expires_at = excluded.claim_expires_at,
    attempts = sends.attempts + 1,
    updated_at = now()
WHERE sends.state = 'sending' AND sends.claim_expires_at <= now()
RETURNING attempts
"""

_CLAIM_STATE = """
SELECT state, greatest(claim_expires_at - now(), interval '0')
FROM sends
WHERE event_id = %s
"""

_SENT = """
UPDATE sends SET state = 'sent', updated_at = now()
WHERE event_id = %s AND claim_id = %s AND state = 'sending'
"""

_REFUSED = """
UPDATE sends SET state = 'failed', reply_code = %s, updated_at = now()
WHERE event_id = %s AND claim_id = %s AND state = 'sending'
"""

_UNDELIVERABLE_STATE = """
SELECT undeliverable_at IS NOT NULL FROM contacts WHERE order_id = %s
"""

_UNDELIVERABLE = """
UPDATE contacts SET undeliverable_at = now()
WHERE order_id = %s AND undeliverable_at IS NULL
"""

_RELEASE = """
DELETE FROM sends WHERE event_id = %s AND claim_id = %s AND state = 'sending'
"""

_SWEEP_SENDS = """
DELETE FROM sends WHERE event_id IN (
    SELECT event_id FROM sends
    WHERE event_time < now() - %s
    LIMIT %s FOR UPDATE SKIP LOCKED
)
"""

_SWEEP_CONTACTS = """
DELETE FROM contacts WHERE order_id IN (
    SELECT order_id FROM contacts
    WHERE delete_after < now()
    LIMIT %s FOR UPDATE SKIP LOCKED
)
"""
