"""JSON logs with the trace ID, and the admin port."""

import io
import json
import logging
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider

from labflow import admin, logs, telemetry


def test_logs_are_json_with_the_service_and_trace(monkeypatch):
    """A record is one JSON line with the service, its fields and the trace."""
    stream = io.StringIO()
    monkeypatch.setattr(logging.getLogger(), "handlers", [])
    log = logs.setup("test-service", logging.INFO, stream)
    tracer = TracerProvider().get_tracer("test")
    with tracer.start_as_current_span("work") as span:
        log.warning("held a message", extra={"event_id": "e1"})
    log.debug("hidden")
    lines = stream.getvalue().splitlines()
    assert len(lines) == 1
    entry = json.loads(lines[0])
    assert entry["level"] == "WARN"
    assert entry["msg"] == "held a message"
    assert entry["service"] == "test-service"
    assert entry["event_id"] == "e1"
    assert entry["trace_id"] == format(span.get_span_context().trace_id, "032x")
    assert entry["time"].endswith("Z")


@pytest.mark.parametrize(
    "name, want", [("debug", logging.DEBUG), ("WARN", logging.WARNING)]
)
def test_parse_level(name, want):
    """LOG_LEVEL takes Go's level names, in any case."""
    assert logs.parse_level(name) == want


def test_parse_level_refuses_unknown_levels():
    """An unknown level is a configuration error."""
    with pytest.raises(ValueError, match="invalid log level"):
        logs.parse_level("verbose")


def status(url: str) -> int:
    """The HTTP status a GET of url answers."""
    try:
        with urllib.request.urlopen(url, timeout=2) as response:
            return response.status
    except urllib.error.HTTPError as e:
        return e.code


def test_admin_server():
    """/healthz answers while the process runs, /readyz follows readiness, and
    the health check passes only when ready."""
    ready = {"value": False}
    server = admin.AdminServer("127.0.0.1:0", lambda: ready["value"])
    server.start()
    try:
        base = f"http://127.0.0.1:{server.port}"
        assert status(base + "/healthz") == 204
        assert status(base + "/readyz") == 503
        with pytest.raises(urllib.error.HTTPError):
            admin.probe(f":{server.port}")
        ready["value"] = True
        assert status(base + "/readyz") == 204
        admin.probe(f":{server.port}")
        assert status(base + "/metrics") == 200
        assert status(base + "/other") == 404
    finally:
        server.stop()


def test_flush_gives_up_after_five_seconds(monkeypatch):
    """A collector that accepts spans but never answers cannot hold shutdown
    past five seconds."""
    with socket.socket() as collector:
        collector.bind(("127.0.0.1", 0))
        collector.listen()
        port = collector.getsockname()[1]
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", f"http://127.0.0.1:{port}")
        monkeypatch.delenv("OTEL_TRACES_EXPORTER", raising=False)
        flush = telemetry.setup("test-service", "1")
        with trace.get_tracer("test").start_as_current_span("work"):
            pass
        start = time.monotonic()
        flush()
        assert time.monotonic() - start < 6.5, "flush outlived its five seconds"


EXIT_PROBE = """
import socket, sys, time
from opentelemetry import trace
from labflow import telemetry
collector = socket.socket()
collector.bind(("127.0.0.1", 0))
collector.listen()
import os
os.environ["OTEL_EXPORTER_OTLP_ENDPOINT"] = f"http://127.0.0.1:{collector.getsockname()[1]}"
flush = telemetry.setup("exit-probe", "1")
with trace.get_tracer("t").start_as_current_span("work"):
    pass
flush()
"""


def test_exit_does_not_wait_on_the_exporter():
    """Once flush has given up, the process exits at once: the SDK's own exit
    hook would wait on the silent collector again."""
    start = time.monotonic()
    subprocess.run([sys.executable, "-c", EXIT_PROBE], check=True, timeout=60)
    assert time.monotonic() - start < 9, "exit waited on the exporter"
