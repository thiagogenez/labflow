"""The consumer against a real RabbitMQ with Docker Compose's definitions: the
email-worker queue, its policy (delivery limit 5) and its dead-letter queue."""

import logging
import socket
import threading
import time
from datetime import timedelta

import pika
import pytest
from prometheus_client import REGISTRY

from labflow import containers, rabbitmq
from labflow.rabbitmq import Consumer, Delivery, Outage, Reject, Retry

QUEUE = "email-worker"
DEAD_LETTER = "email-worker.dead-letter"


@pytest.fixture(scope="module", name="broker")
def fixture_broker():
    """One broker for the module's tests."""
    with containers.rabbitmq() as broker:
        yield broker


@pytest.fixture(name="admin")
def fixture_admin(broker):
    """An administrator's channel, with both queues emptied."""
    connection = broker.admin()
    channel = connection.channel()
    channel.queue_purge(QUEUE)
    channel.queue_purge(DEAD_LETTER)
    yield channel
    connection.close()


def publish(channel, body: bytes = b"{}") -> None:
    """Publishes as order-api does, to the email-worker queue's binding."""
    channel.basic_publish(
        "labflow.events",
        "labflow.order.placed.v1",
        body,
        pika.BasicProperties(delivery_mode=2),
    )


def get(channel, queue: str, timeout: float = 15):
    """The properties and body of the next message in queue."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        method, properties, body = channel.basic_get(queue, auto_ack=True)
        if method is not None:
            return properties, body
        time.sleep(0.2)
    raise AssertionError(f"no message in {queue}")


def empty(channel, queue: str) -> bool:
    """Whether queue holds no message."""
    return channel.queue_declare(queue, passive=True).method.message_count == 0


def wait_for(
    condition, timeout: float = 15, what: str = "condition not met in time"
) -> None:
    """Waits until condition() holds, failing with what."""
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError(what)
        time.sleep(0.1)


class Running:
    """A consumer running in a thread until the block ends."""

    def __init__(self, url, handler, queue=QUEUE, ready=True):
        self.stop = threading.Event()
        self.consumer = Consumer(url, queue, handler, logging.getLogger("test"))
        self.thread = threading.Thread(target=self.consumer.run, args=(self.stop,))
        self.ready = ready

    def __enter__(self):
        self.thread.start()
        if self.ready:
            wait_for(self.consumer.ready)
        return self

    def __exit__(self, *_):
        self.stop.set()
        self.thread.join(timeout=30)
        assert not self.thread.is_alive(), "the consumer did not stop"


def test_a_handled_message_is_acknowledged(broker, admin):
    """A handler that returns acknowledges the message."""
    handled = []
    with Running(broker.url("email-worker"), lambda d: handled.append(d.body)):
        publish(admin, b'{"n": 1}')
        wait_for(lambda: handled == [b'{"n": 1}'])
    assert empty(admin, QUEUE) and empty(admin, DEAD_LETTER)


def test_a_rejected_message_goes_to_the_dead_letter_queue_at_once(broker, admin):
    """Reject dead-letters the message on its first delivery."""
    calls = []

    def handler(delivery: Delivery) -> None:
        calls.append(delivery)
        raise Reject("cannot be parsed")

    with Running(broker.url("email-worker"), handler):
        publish(admin, b"not json")
        properties, body = get(admin, DEAD_LETTER)
    assert body == b"not json"
    assert properties.headers["x-first-death-reason"] == "rejected"
    assert len(calls) == 1


def test_a_failing_message_is_dead_lettered_at_the_delivery_limit(broker, admin):
    """An unexpected error counts as a failed delivery, so the queue's
    delivery limit of 5 dead-letters the message."""
    calls = []

    def handler(_: Delivery) -> None:
        calls.append(1)
        raise RuntimeError("a bug")

    with Running(broker.url("email-worker"), handler):
        publish(admin)
        properties, _ = get(admin, DEAD_LETTER, timeout=30)
    assert properties.headers["x-first-death-reason"] == "delivery_limit"
    assert len(calls) == 6  # the first delivery and 5 failed redeliveries


def test_an_outage_holds_the_message_and_reports_not_ready(broker, admin):
    """An outage keeps the delivery, retries the handler with the same state,
    and reports not ready until a retry succeeds."""
    attempts = []

    def handler(delivery: Delivery) -> None:
        attempts.append(delivery.state.setdefault("token", object()))
        if len(attempts) < 3:
            raise Outage("database unavailable")

    with Running(broker.url("email-worker"), handler) as running:
        publish(admin)
        wait_for(lambda: len(attempts) == 1)
        assert not running.consumer.ready()
        wait_for(lambda: len(attempts) == 3)
        wait_for(running.consumer.ready)
    assert len({id(token) for token in attempts}) == 1
    assert empty(admin, QUEUE) and empty(admin, DEAD_LETTER)


@pytest.mark.parametrize("hold", [Outage("mail server unavailable"), None])
def test_stopping_returns_messages_without_a_failed_delivery(broker, admin, hold):
    """Stopping returns both the message held through an outage and those
    queued behind it, none counted as a failed delivery, so restarts never
    use up the delivery limit."""
    started = threading.Event()

    def handler(_: Delivery) -> None:
        started.set()
        if hold:
            raise hold
        time.sleep(2)  # still handling the first message when stopped

    with Running(broker.url("email-worker"), handler):
        for _ in range(3):
            publish(admin)
        assert started.wait(15)
    for _ in range(3 if hold else 2):
        properties, _ = get(admin, QUEUE)
        assert (properties.headers or {}).get("x-delivery-count", 0) == 0


def test_a_retry_holds_the_message_while_staying_ready(broker, admin):
    """Retry calls the handler again after the given time, still ready."""
    attempts = []

    def handler(_: Delivery) -> None:
        attempts.append(time.monotonic())
        if len(attempts) == 1:
            raise Retry("another worker owns it", timedelta(seconds=1))

    with Running(broker.url("email-worker"), handler) as running:
        publish(admin)
        wait_for(lambda: len(attempts) == 1)
        assert running.consumer.ready()
        wait_for(lambda: len(attempts) == 2)
    assert attempts[1] - attempts[0] >= 1
    assert empty(admin, QUEUE)


def test_a_long_handler_finishes_before_the_consumer_stops(broker, admin):
    """A message in hand when the stop comes is handled and acknowledged,
    within the shutdown budget, even when it takes longer than pika's default
    10-second close."""
    started, finished = threading.Event(), threading.Event()

    def handler(_: Delivery) -> None:
        started.set()
        time.sleep(12)
        finished.set()

    stopping = 0.0
    with Running(broker.url("email-worker"), handler):
        publish(admin)
        assert started.wait(15)
        stopping = time.monotonic()
    assert finished.is_set(), "the message in hand was cut off"
    assert time.monotonic() - stopping < 25
    assert empty(admin, QUEUE) and empty(admin, DEAD_LETTER)


def test_a_bug_is_logged_without_its_message(broker, admin, caplog):
    """An exception's message may repeat data, such as a database row with an
    email address, so only its type and frames are logged."""

    def handler(_: Delivery) -> None:
        raise RuntimeError("Failing row contains (ada@example.com)")

    with caplog.at_level(logging.ERROR), Running(broker.url("email-worker"), handler):
        publish(admin)
        get(admin, DEAD_LETTER, timeout=30)
    records = [
        r for r in caplog.records if r.getMessage() == "failed to process a message"
    ]
    assert records and all(r.error_type == "RuntimeError" for r in records)
    assert not any("ada@example.com" in str(vars(r)) for r in caplog.records)


def test_a_cancelled_subscription_is_restored(broker, admin):
    """When the broker cancels the subscription, here because the queue was
    deleted, the consumer subscribes again once it can, without a restart."""
    handled = []
    with Running(
        broker.url("email-worker"), lambda d: handled.append(d.body)
    ) as running:
        admin.queue_delete(QUEUE)
        wait_for(lambda: not running.consumer.ready())
        admin.queue_declare(QUEUE, durable=True, arguments={"x-queue-type": "quorum"})
        admin.queue_bind(QUEUE, "labflow.events", "labflow.order.placed.v1")
        wait_for(running.consumer.ready, timeout=40)
        publish(admin, b"after")
        wait_for(lambda: handled == [b"after"])


def test_a_broker_name_that_fails_to_resolve_is_retried_until_it_does(
    broker, admin, monkeypatch
):
    """A failure to resolve the broker's name, which pika raises outside its
    own error classes, is retried; once the name resolves, the consumer
    subscribes and handles messages."""
    real, failures = socket.getaddrinfo, []

    def flaky(host, *args, **kwargs):
        if host == "broker.test":
            if not failures:
                failures.append(host)
                raise socket.gaierror("temporary failure in name resolution")
            host = "127.0.0.1"
        return real(host, *args, **kwargs)

    monkeypatch.setattr(socket, "getaddrinfo", flaky)
    url = broker.url("email-worker").replace("@127.0.0.1:", "@broker.test:")
    handled = []
    with Running(url, lambda d: handled.append(d.body), ready=False) as running:
        wait_for(
            running.consumer.ready, timeout=20, what="the consumer never subscribed"
        )
        assert running.thread.is_alive(), "the consumer died"
        publish(admin, b"resolved")
        wait_for(lambda: handled == [b"resolved"])
    assert failures == ["broker.test"]


def test_a_refused_subscription_is_retried_with_backoff_until_it_works(
    broker, admin, caplog
):
    """A broker that refuses the subscription, here to a queue that does not
    exist yet, is retried with backoff, not in a tight loop, and the consumer
    subscribes and handles messages once the queue exists."""
    queue, handled = "late-queue", []
    try:
        with caplog.at_level(logging.WARNING), Running(
            broker.url("admin"),
            lambda d: handled.append(d.body),
            queue=queue,
            ready=False,
        ) as running:
            time.sleep(3.5)
            refused = [
                r
                for r in caplog.records
                if r.getMessage() == "no RabbitMQ subscription"
            ]
            assert 1 <= len(refused) <= 3, f"{len(refused)} attempts in 3.5 seconds"
            # NOT_FOUND, told apart from ACCESS_REFUSED, without the free text
            assert refused[0].reply_code == 404
            assert not hasattr(
                refused[0], "reply_text"
            ), "free-form reply text was logged"
            admin.queue_declare(
                queue, durable=True, arguments={"x-queue-type": "quorum"}
            )
            wait_for(
                running.consumer.ready, timeout=40, what="the consumer never subscribed"
            )
            admin.basic_publish("", queue, b"late")
            wait_for(lambda: handled == [b"late"])
    finally:
        admin.queue_delete(queue)


def test_an_outage_shows_on_the_holding_gauge(broker, admin):
    """While a message is held through an outage, rabbitmq_consumer_holding is
    1, so an alert sees the outage before any email is late; it is 0 once the
    message is settled."""
    attempts = []

    def handler(_: Delivery) -> None:
        attempts.append(1)
        if len(attempts) < 3:
            raise Outage("mail server unavailable")

    def holding() -> float:
        return REGISTRY.get_sample_value("rabbitmq_consumer_holding", {"queue": QUEUE})

    with Running(broker.url("email-worker"), handler):
        publish(admin)
        wait_for(lambda: len(attempts) == 1)
        assert holding() == 1
        wait_for(lambda: len(attempts) == 3)
        wait_for(lambda: holding() == 0)


def test_a_long_outage_never_reaches_the_consumer_timeout(broker, admin, caplog):
    """The subscription asks for a 7-day consumer timeout, which prevails over
    the queue's 5 seconds, so a message held through a long outage stays in
    hand with what the handler remembers, and the broker never cancels the
    subscription."""
    queue = "consumer-timeout-test"
    admin.queue_declare(
        queue,
        durable=True,
        arguments={"x-queue-type": "quorum", "x-consumer-timeout": 5000},
    )
    outage_ends = time.monotonic() + 15
    handled, states = [], set()

    def handler(delivery: Delivery) -> None:
        states.add(id(delivery.state))
        if time.monotonic() < outage_ends:
            raise Outage("mail server unavailable")
        handled.append(delivery.body)

    try:
        with caplog.at_level(logging.WARNING), Running(
            broker.url("admin"), handler, queue=queue
        ):
            admin.basic_publish("", queue, b"first")
            wait_for(lambda: handled == [b"first"], timeout=40)
            admin.basic_publish("", queue, b"second")
            wait_for(
                lambda: handled == [b"first", b"second"],
                timeout=5,
                what="the next message waited: the subscription was lost",
            )
        assert len(states) == 2, "the held message came back without its state"
        lost = [
            r
            for r in caplog.records
            if r.getMessage() == "lost the RabbitMQ subscription"
        ]
        assert not lost, "the broker cancelled the subscription"
    finally:
        admin.queue_delete(queue)


def test_a_retry_after_an_outage_clears_the_holding_gauge(broker, admin):
    """An outage followed by a wait on another worker's claim is no longer an
    outage: the gauge and readiness recover during the wait."""
    attempts, waiting = [], threading.Event()

    def handler(_: Delivery) -> None:
        attempts.append(1)
        if len(attempts) == 1:
            raise Outage("mail server unavailable")
        if len(attempts) == 2:
            waiting.set()
            raise Retry("another worker owns it", timedelta(seconds=3))

    with Running(broker.url("email-worker"), handler) as running:
        publish(admin)
        assert waiting.wait(15)
        time.sleep(0.3)
        assert running.consumer.ready(), "the wait kept the outage state"
        assert (
            REGISTRY.get_sample_value("rabbitmq_consumer_holding", {"queue": QUEUE})
            == 0
        )


def test_a_lost_connection_waits_for_the_message_in_hand(broker, admin, monkeypatch):
    """When the connection is lost while a message is being handled, the next
    subscription starts only once that handler has finished, however long it
    takes, so the two never share the handler or overwrite each other's
    readiness."""
    monkeypatch.setattr(rabbitmq, "SHUTDOWN_BUDGET", 1.0)
    release, started, finished = threading.Event(), threading.Event(), []

    def handler(delivery: Delivery) -> None:
        if delivery.body == b"slow":
            started.set()
            release.wait(30)
            finished.append(time.monotonic())

    with Running(broker.url("email-worker"), handler) as running:
        publish(admin, b"slow")
        assert started.wait(15)
        broker.close_connections()
        time.sleep(5)  # well past the budget, which a stop would start
        assert not running.consumer.ready(), "subscribed again while a handler ran"
        released = time.monotonic()
        release.set()
        wait_for(running.consumer.ready, timeout=30)
        assert finished and finished[0] >= released


def test_a_stop_after_a_lost_connection_still_waits_for_the_handler(
    broker, admin, monkeypatch
):
    """The shutdown budget counts from the stop, not from the lost
    connection: a stop that comes later still gives the handler in hand its
    whole budget to finish."""
    monkeypatch.setattr(rabbitmq, "SHUTDOWN_BUDGET", 2.0)
    release, started = threading.Event(), threading.Event()

    def handler(delivery: Delivery) -> None:
        if delivery.body == b"slow":
            started.set()
            release.wait(30)

    running = Running(broker.url("email-worker"), handler)
    with running:
        publish(admin, b"slow")
        assert started.wait(15)
        broker.close_connections()
        time.sleep(3)  # past a budget counted from the lost connection
        running.stop.set()
        time.sleep(1)
        assert running.thread.is_alive(), "stopped while the handler still ran"
        release.set()
