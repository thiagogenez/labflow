"""A RabbitMQ consumer that tells a broken message from a broken dependency.

A handler's outcome decides what happens to the delivery:

- it returns: the delivery is acknowledged;
- it raises Reject: the message can never be processed, such as one that
  cannot be parsed, and is rejected straight to the dead-letter queue;
- it raises Outage: a dependency is unavailable, so the consumer keeps the
  delivery and calls the handler again with backoff from 1 to 30 seconds,
  reporting not ready meanwhile;
- it raises Retry: the message must wait, such as for another worker, or for
  the consumer to stop, so the consumer keeps it and calls the handler again
  after the given time;
- it raises anything else: a bug, so the delivery is rejected back to its
  queue, which counts as a failed delivery and dead-letters the message at
  the queue's delivery limit.

The subscription asks RabbitMQ for a consumer timeout of 7 days, the most an
event is processed for (D64), so a message is held through an outage of any
length without the broker taking it back. A graceful stop returns any message
in hand with basic.nack, which RabbitMQ 4.3 and later do not count as a
failed delivery, so neither an outage nor a wait uses up the delivery limit.
"""

import logging
import re
import ssl
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any
from urllib.parse import quote, unquote, urlsplit

import pika
import pika.adapters.thread_safe_connection
from pika.spec import Basic
from prometheus_client import Counter, Gauge

from labflow.config import Env, split_url
from labflow.errors import capture, error_fields

_MIN_BACKOFF = 1.0
_MAX_BACKOFF = 30.0
_TICK = 1.0
# What pika's close may spend, at most twice, for the delivery in hand to
# finish and the connection to close: email-worker's grace period is 105
# seconds.
SHUTDOWN_BUDGET = 45.0
CONSUMER_TIMEOUT = timedelta(days=7)
# A percent sign that does not start an escape such as %2F.
_BAD_ESCAPE = re.compile(r"%(?![0-9A-Fa-f]{2})")


class Reject(Exception):
    """The message can never be processed; it goes to the dead-letter queue."""


class Outage(Exception):
    """A dependency is unavailable; the message waits until it is back."""


class Retry(Exception):
    """The message must wait; the handler is called again after `after`."""

    def __init__(self, reason: str, after: timedelta):
        super().__init__(reason)
        self.after = after


DELIVERIES = Counter(
    "rabbitmq_consumer_deliveries_total",
    "Deliveries settled, by queue and outcome: acked, dead_lettered (rejected"
    " as unprocessable), failed (rejected back to the queue, counting towards"
    " its delivery limit) or returned (on a graceful stop).",
    ["queue", "outcome"],
)
HOLDING = Gauge(
    "rabbitmq_consumer_holding",
    "1 while the consumer holds a message through a dependency's outage, else 0.",
    ["queue"],
)


def url_from_env(env: Env) -> str:
    """Reads BROKER, RABBITMQ_URL without its password, and the password from
    the file RABBITMQ_PASSWORD_FILE names. A URL with a password or any query
    option, which could carry one such as a TLS key's password, is refused,
    and no error repeats either value."""
    broker = env.required("BROKER")
    env.check(broker in ("", "rabbitmq"), f"BROKER must be rabbitmq, got {broker!r}")
    raw = env.required("RABBITMQ_URL")
    password = env.secret("RABBITMQ_PASSWORD")
    if not raw:
        return ""
    u = split_url(raw, ("amqp", "amqps"))
    if (
        u is None
        or not u.username
        or any(_BAD_ESCAPE.search(part) for part in (u.path, u.netloc))
    ):
        env.check(
            False,
            "RABBITMQ_URL must be an amqp:// or amqps:// URL"
            " with a host, a user and a valid port",
        )
        return ""
    if u.password is not None or u.query or u.fragment:
        env.check(
            False,
            "RABBITMQ_URL must carry no password, query or fragment;"
            " its secret file holds the password",
        )
        return ""
    netloc = f"{u.username}:{quote(password, safe='')}@{u.netloc.rpartition('@')[2]}"
    return u._replace(netloc=netloc).geturl()


def parameters(url: str) -> pika.ConnectionParameters:
    """Connection parameters for a URL that url_from_env accepted. The virtual
    host is the whole path, as the Go services read it: /team/orders is
    team/orders, where pika would stop at the first slash."""
    u = urlsplit(url)
    tls = u.scheme == "amqps"
    return pika.ConnectionParameters(
        host=u.hostname,
        port=u.port or (5671 if tls else 5672),
        virtual_host=unquote(u.path[1:]) if u.path not in ("", "/") else "/",
        credentials=pika.PlainCredentials(unquote(u.username), unquote(u.password)),
        ssl_options=(
            pika.SSLOptions(ssl.create_default_context(), u.hostname) if tls else None
        ),
    )


@dataclass
class Delivery:
    """One message, handed to the handler on every attempt until it is
    settled. state keeps what the handler must remember between attempts;
    stopping is set once the consumer stops, so a long handler can return the
    message at a safe point instead of being cut off."""

    body: bytes
    state: dict[str, Any] = field(default_factory=dict)
    stopping: threading.Event = field(default_factory=threading.Event)


Handler = Callable[[Delivery], None]


def _set(event: threading.Event) -> threading.Event:
    event.set()
    return event


@dataclass
class _Session:
    """One subscription: the events its threads coordinate through."""

    stop: threading.Event
    # Set when the broker cancelled the subscription or the channel closed.
    lost: threading.Event = field(default_factory=threading.Event)
    # Clear while a delivery is being settled, on pika's consumer thread.
    idle: threading.Event = field(default_factory=lambda: _set(threading.Event()))
    # Whether the subscription was ever in place.
    subscribed: bool = False


class Consumer:
    """Consumes one queue, one message at a time.

    pika's thread-safe Connection keeps the connection, heartbeats included,
    on a thread of its own and calls the handler on another, so a message held
    through an outage never stalls the connection. ready() is safe to call
    from any thread."""

    def __init__(self, url: str, queue: str, handler: Handler, log: logging.Logger):
        self._parameters = parameters(url)
        self._queue = queue
        self._handler = handler
        self._log = log
        self._subscribed = threading.Event()
        self._holding = threading.Event()
        HOLDING.labels(queue).set(0)

    def ready(self) -> bool:
        """Whether the consumer is subscribed and not waiting out an outage."""
        return self._subscribed.is_set() and not self._holding.is_set()

    def run(self, stop: threading.Event) -> None:
        """Consumes until stop is set, subscribing again after any failure to
        connect or subscribe, or any loss of the subscription, with backoff."""
        backoff = _MIN_BACKOFF
        while not stop.is_set():
            session = _Session(stop)
            if (error := capture(self._subscribe, session)) is not None:
                self._log.warning("no RabbitMQ subscription", extra=error_fields(error))
            if session.subscribed:
                backoff = _MIN_BACKOFF
            stop.wait(backoff)
            backoff = min(backoff * 2, _MAX_BACKOFF)

    def _subscribe(self, session: _Session) -> None:
        connection = pika.Connection(self._parameters)
        stopped_at = None
        try:
            channel = connection.channel()
            channel.add_on_cancel_callback(lambda _: session.lost.set())
            channel.basic_qos(prefetch_count=1)
            channel.basic_consume(
                self._queue,
                lambda ch, method, _properties, body: self._settle(
                    ch, method, Delivery(body, stopping=session.stop), session
                ),
                arguments={
                    "x-consumer-timeout": int(CONSUMER_TIMEOUT.total_seconds() * 1000)
                },
            )
            self._subscribed.set()
            session.subscribed = True
            while not session.stop.wait(_TICK):
                if session.lost.is_set() or not channel.is_open:
                    session.lost.set()
                    self._log.warning("lost the RabbitMQ subscription")
                    return
            stopped_at = time.monotonic()
        finally:
            self._subscribed.clear()
            # pika cancels the subscription, then runs the deliveries already
            # queued, which return their messages, before it closes; a
            # connection already broken has nothing left to close.
            capture(connection.close, timeout=SHUTDOWN_BUDGET)
            # A delivery still being settled on a lost connection finishes
            # before the next subscription shares the handler with another;
            # once stopping, the consumer waits only for what is left of its
            # budget, counted from the stop.
            while not session.idle.wait(_TICK):
                if session.stop.is_set():
                    stopped_at = stopped_at or time.monotonic()
                    if time.monotonic() - stopped_at > SHUTDOWN_BUDGET:
                        break

    def _settle(
        self,
        channel: pika.adapters.thread_safe_connection.Channel,
        method: Basic.Deliver,
        delivery: Delivery,
        session: _Session,
    ) -> None:
        """Runs on the consumer thread until the delivery is settled, the
        subscription is lost, or the consumer stops."""
        session.idle.clear()
        try:
            error = capture(
                self._process, channel, method.delivery_tag, delivery, session
            )
            if error is not None:
                # The channel or connection is gone: the broker returns the
                # message itself, as a failed delivery.
                self._log.warning(
                    "could not settle a message", extra=error_fields(error)
                )
            self._hold_state(False)
        finally:
            session.idle.set()

    def _process(
        self,
        channel: pika.adapters.thread_safe_connection.Channel,
        tag: int,
        delivery: Delivery,
        session: _Session,
    ) -> None:
        backoff = _MIN_BACKOFF
        while not session.stop.is_set():
            wait = 0.0
            match capture(self._handler, delivery):
                case None:
                    channel.basic_ack(tag)
                    self._settled("acked")
                    return
                case Reject() as e:
                    self._log.warning("rejected a message", extra={"reason": str(e)})
                    channel.basic_reject(tag, requeue=False)
                    self._settled("dead_lettered")
                    return
                case Outage() as e:
                    self._hold_state(True)
                    self._log.warning(
                        "holding a message through an outage",
                        extra={"reason": str(e), "retry_in_seconds": backoff},
                    )
                    wait, backoff = backoff, min(backoff * 2, _MAX_BACKOFF)
                case Retry(after=after) as e:
                    self._hold_state(False)  # waiting, not an outage
                    wait = after.total_seconds()
                    self._log.info(
                        "holding a message",
                        extra={"reason": str(e), "retry_in_seconds": wait},
                    )
                case e:
                    # A bug: the broker counts the failed delivery and
                    # dead-letters the message at the queue's delivery limit.
                    # The exception's message may repeat data, so it is left out.
                    self._log.error(
                        "failed to process a message", extra=error_fields(e)
                    )
                    channel.basic_reject(tag, requeue=True)
                    self._settled("failed")
                    return
            if not self._hold(wait, session):
                break
        if session.lost.is_set():
            return  # the broker has taken the message back already
        channel.basic_nack(tag, requeue=True)
        self._settled("returned")

    def _settled(self, outcome: str) -> None:
        DELIVERIES.labels(self._queue, outcome).inc()

    def _hold_state(self, holding: bool) -> None:
        (self._holding.set if holding else self._holding.clear)()
        HOLDING.labels(self._queue).set(int(holding))

    @staticmethod
    def _hold(seconds: float, session: _Session) -> bool:
        """Waits up to seconds; False once the consumer stops or the
        subscription is lost."""
        deadline = time.monotonic() + seconds
        while (remaining := deadline - time.monotonic()) > 0:
            if session.stop.wait(min(remaining, _TICK)) or session.lost.is_set():
                return False
        return not session.lost.is_set()
