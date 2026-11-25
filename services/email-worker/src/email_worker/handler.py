"""Turns order events into emails, at least once and at most once per claim
(D69)."""

import logging
import threading
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from email.errors import MessageError
from email.message import EmailMessage

from labflow import events
from labflow.errors import capture, error_fields
from labflow.rabbitmq import Delivery, Outage, Reject, Retry
from opentelemetry import trace
from opentelemetry.trace import Status, StatusCode
from prometheus_client import Counter, Histogram

from email_worker import render
from email_worker.links import Links
from email_worker.mail import Mailer, MaybeSent, NotSent, Refused
from email_worker.store import Busy, Claimed, Settled, Store

ORDER_PLACED = "labflow.order.placed.v1"
# Processing ends 7 days after the event's time (D64).
MAX_AGE = timedelta(days=7)

EMAILS = Counter(
    "email_worker_emails_total",
    "Events handled, by outcome: sent; refused for good by the mail server;"
    " undeliverable, skipped because an earlier email to the order's contact"
    " was refused; settled already; or expired after 7 days.",
    ["outcome"],
)
RESENDS = Counter(
    "email_worker_resend_attempts_total",
    "Sends that took over an expired claim, so the email may arrive twice.",
)
DELAY = Histogram(
    "email_worker_email_delay_seconds",
    "Time from an event to the mail server accepting its email.",
    buckets=(1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 300, 600, 1800),
)
SWEPT = Counter(
    "email_worker_rows_deleted_total",
    "Rows the hourly sweep deleted, by table.",
    ["table"],
)

_tracer = trace.get_tracer("email_worker")


@dataclass
class Handler:
    """Handles one delivery at a time, on the consumer's thread. What the mail
    server answered is kept in the delivery's state, so a retry after a
    database outage records that answer instead of sending again."""

    store: Store
    mailer: Mailer
    links: Links
    log: logging.Logger

    def __call__(self, delivery: Delivery) -> None:
        event = events.parse(delivery.body)
        # Exceptions are recorded by type only: their messages may repeat data
        # such as a database row.
        with _tracer.start_as_current_span(
            "process email-worker",
            context=event.context(),
            kind=trace.SpanKind.CONSUMER,
            attributes={
                "messaging.system": "rabbitmq",
                "messaging.operation.type": "process",
                "messaging.message.id": event.id,
                "cloudevents.event_type": event.type,
            },
            record_exception=False,
            set_status_on_exception=False,
        ) as span:
            try:
                self._handle(event, delivery)
            except Exception as e:
                span.set_status(Status(StatusCode.ERROR, type(e).__name__))
                raise

    def _handle(self, event: events.Event, delivery: Delivery) -> None:
        state = delivery.state
        fields = {
            "event_id": event.id,
            "event_type": event.type,
            "subject": event.subject,
        }
        # What the mail server answered, and a claim still owed back, are
        # settled whatever the event's age.
        if state.get("release"):
            self.store.release(event.id, state["claim_id"])
            del state["release"]
        if self._record(event.id, state, fields):
            return
        if event.type != ORDER_PLACED:
            raise Reject(f"email-worker handles no {event.type}")
        if datetime.now(UTC) - event.time > MAX_AGE:
            EMAILS.labels("expired").inc()
            self.log.warning("skipped an event older than 7 days", extra=fields)
            return
        order_id, email, first_name, message = self._prepare(event)
        claim_id = state.setdefault("claim_id", uuid.uuid4())
        # A stopping worker returns the message, uncounted, before each step
        # that could outlast its grace period: every database call and the
        # send, giving back a claim it took. Once sent, the email is recorded.
        self._unless_stopping(delivery)
        self.store.save_contact(order_id, email, first_name, event.time)
        self._unless_stopping(delivery)
        if self.store.undeliverable(order_id):
            EMAILS.labels("undeliverable").inc()
            self.log.info("skipped an email to a refused contact", extra=fields)
            return
        self._unless_stopping(delivery)
        if not self._claim(event, order_id, claim_id, state, fields):
            return
        state["order_id"] = order_id
        if delivery.stopping.is_set():
            self.store.release(event.id, claim_id)
            self._unless_stopping(delivery)
        self._send(event, message, state, fields)
        self._record(event.id, state, fields)

    def _send(
        self, event: events.Event, message: EmailMessage, state: dict, fields: dict
    ) -> None:
        """Sends the email and keeps what the mail server answered in state;
        an outage gives the claim back, or keeps it if the email may have
        been accepted."""
        try:
            self.mailer.send(message)
        except Refused as e:
            state["refused"] = (e.code, e.stage)
        except NotSent:
            try:
                self.store.release(event.id, state["claim_id"])
            except Outage:
                state["release"] = True  # before the next attempt
            raise
        except MaybeSent:
            if not state.get("takeover"):
                raise  # the claim stays: after it expires, a takeover resends
            # A resend that again gets no answer is recorded as sent: a server
            # that accepts too slowly gets two copies, not one every 2 minutes.
            state["accepted"] = True
            self.log.warning(
                "no answer to a resent email; recorded as sent", extra=fields
            )
        else:
            state["accepted"] = True
            DELAY.observe(max((datetime.now(UTC) - event.time).total_seconds(), 0))

    def _record(self, event_id: str, state: dict, fields: dict) -> bool:
        """Records what the mail server answered, if it has; True once done.
        Only the claim's owner records anything."""
        claim_id, order_id = state.get("claim_id"), state.get("order_id")
        if state.get("accepted"):
            if self.store.sent(event_id, claim_id):
                EMAILS.labels("sent").inc()
                self.log.info("sent an email", extra=fields)
            else:
                self.log.warning("sent an email after losing its claim", extra=fields)
            return True
        if (refusal := state.get("refused")) is not None:
            code, stage = refusal
            # Only a refused recipient is a fault of the address; a refused
            # sender or message is not, and later emails may go through.
            if self.store.refused(
                event_id, claim_id, order_id, code, stage == "recipient"
            ):
                EMAILS.labels("refused").inc()
            self.log.warning(
                "the mail server refused the email for good",
                extra=fields | {"reply_code": code},
            )
            return True
        return False

    @staticmethod
    def _unless_stopping(delivery: Delivery) -> None:
        if delivery.stopping.is_set():
            raise Retry("the worker is stopping", timedelta(0))

    def _prepare(self, event: events.Event) -> tuple[str, str, str, EmailMessage]:
        """Returns the order ID, the contact and the email, or raises Reject
        when the event lacks what the email needs."""
        try:
            data = event.data
            uuid.UUID(event.id)
            order_id = str(uuid.UUID(render.text(data["order_id"])))
            email = render.text(data["customer"]["email"])
            first_name = render.text(data["customer"]["first_name"])
            link = self.links.order(order_id)
            message = render.order_confirmation(event.id, datetime.now(UTC), data, link)
        except (KeyError, TypeError, ValueError, MessageError) as e:
            raise Reject(
                f"order.placed without what its email needs: {type(e).__name__}"
            ) from None
        return order_id, email, first_name, message

    def _claim(
        self,
        event: events.Event,
        order_id: str,
        claim_id: uuid.UUID,
        state: dict,
        fields: dict,
    ) -> bool:
        """Claims the send; False when it is settled already, Retry while
        another worker owns it."""
        claim = self.store.claim(event.id, event.type, order_id, event.time, claim_id)
        match claim:
            case Settled():
                EMAILS.labels("settled").inc()
                self.log.info(
                    "skipped an email already settled",
                    extra=fields | {"state": claim.state},
                )
                return False
            case Busy():
                raise Retry(
                    "another worker is sending this email",
                    claim.remaining + timedelta(seconds=1),
                )
            case Claimed() if claim.attempts > 1:
                state["takeover"] = True
                RESENDS.inc()
                self.log.warning(
                    "took over an expired claim; the email may arrive twice",
                    extra=fields,
                )
        return True


_SWEEP_EVERY = 3600.0
_SWEEP_RETRY = 30.0


@dataclass
class Sweeper:
    """Deletes expired sends and contacts once an hour, on a thread and a
    database connection of its own, whether or not the broker answers. A
    failed sweep is retried every 30 seconds and makes the worker not ready
    until one succeeds."""

    store: Store
    log: logging.Logger
    _healthy: threading.Event = field(default_factory=threading.Event)

    def healthy(self) -> bool:
        """Whether the last sweep reached the database."""
        return self._healthy.is_set()

    def _sweep(self) -> None:
        for table, count in self.store.sweep().items():
            SWEPT.labels(table).inc(count)

    def run(self, stop: threading.Event) -> None:
        """Sweeps at once, then hourly, until stop is set. Any failure, not
        only an outage (a missing grant or column after a migration, say),
        is logged by type and retried, with the worker not ready meanwhile,
        so retention never stops silently."""
        while True:
            error = capture(self._sweep)
            if error is None:
                self._healthy.set()
                wait = _SWEEP_EVERY
            else:
                self._healthy.clear()
                self.log.warning("the sweep failed", extra=error_fields(error))
                wait = _SWEEP_RETRY
            if stop.wait(wait):
                return
