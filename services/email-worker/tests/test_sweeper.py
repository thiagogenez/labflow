"""The hourly sweep's thread and the readiness it reports."""

import logging
import threading
import time

from labflow.rabbitmq import Outage

from email_worker import handler
from email_worker.handler import Sweeper


def wait_for(condition, timeout: float = 10) -> None:
    """Waits until condition() holds."""
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        time.sleep(0.05)


def test_a_sweep_runs_at_once_and_the_thread_stops_on_request(store):
    """The first sweep runs at start, makes the worker healthy, and stopping
    ends the hourly wait at once."""
    sweeper, stop = Sweeper(store, logging.getLogger("test")), threading.Event()
    thread = threading.Thread(target=sweeper.run, args=(stop,), daemon=True)
    assert not sweeper.healthy()
    thread.start()
    try:
        wait_for(sweeper.healthy)
    finally:
        stop.set()
        thread.join(2)
    assert not thread.is_alive()


def test_a_sweep_that_fails_later_reports_unhealthy(store, monkeypatch):
    """A database lost after a good sweep makes the worker not ready at the
    next sweep."""
    monkeypatch.setattr(handler, "_SWEEP_EVERY", 0.1)
    monkeypatch.setattr(handler, "_SWEEP_RETRY", 0.1)
    sweeper, stop = Sweeper(store, logging.getLogger("test")), threading.Event()
    thread = threading.Thread(target=sweeper.run, args=(stop,), daemon=True)
    thread.start()
    try:
        wait_for(sweeper.healthy)

        def down():
            raise Outage("database: OperationalError")

        monkeypatch.setattr(store, "sweep", down)
        wait_for(lambda: not sweeper.healthy())
    finally:
        stop.set()
        thread.join(2)


def test_an_unexpected_sweep_error_keeps_the_thread_and_reports_unhealthy(
    store, monkeypatch, caplog
):
    """An error that is no outage, such as a grant lost in a migration, is
    logged by type only and retried, with the worker not ready meanwhile:
    retention never stops silently."""
    monkeypatch.setattr(handler, "_SWEEP_EVERY", 0.1)
    monkeypatch.setattr(handler, "_SWEEP_RETRY", 0.1)
    sweeper, stop = Sweeper(store, logging.getLogger("test")), threading.Event()
    thread = threading.Thread(target=sweeper.run, args=(stop,), daemon=True)
    thread.start()
    try:
        wait_for(sweeper.healthy)

        def broken():
            raise RuntimeError("permission denied for table contacts (ada@example.com)")

        with caplog.at_level(logging.WARNING):
            monkeypatch.setattr(store, "sweep", broken)
            wait_for(lambda: not sweeper.healthy())
            time.sleep(0.3)
        assert thread.is_alive(), "the sweep thread died"
        records = [r for r in caplog.records if r.getMessage() == "the sweep failed"]
        assert records and records[0].error_type == "RuntimeError"
        assert "ada@example.com" not in caplog.text
    finally:
        stop.set()
        thread.join(2)
