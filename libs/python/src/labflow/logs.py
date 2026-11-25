"""Structured JSON logs, shaped like the Go services' slog records."""

import json
import logging
import sys
from datetime import UTC, datetime
from typing import TextIO

from opentelemetry import trace

# Attributes every LogRecord has; anything else came from `extra`.
_STANDARD = set(vars(logging.makeLogRecord({}))) | {"message", "asctime", "taskName"}
_LEVELS = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "error": logging.ERROR,
}


class _JSONFormatter(logging.Formatter):
    def __init__(self, service: str):
        super().__init__()
        self._service = service

    def format(self, record: logging.LogRecord) -> str:
        entry = {
            "time": datetime.fromtimestamp(record.created, UTC)
            .isoformat(timespec="milliseconds")
            .replace("+00:00", "Z"),
            "level": "WARN" if record.levelno == logging.WARNING else record.levelname,
            "msg": record.getMessage(),
            "service": self._service,
        }
        context = trace.get_current_span().get_span_context()
        if context.is_valid:
            entry["trace_id"] = format(context.trace_id, "032x")
            entry["span_id"] = format(context.span_id, "016x")
        for key, value in vars(record).items():
            if key not in _STANDARD:
                entry[key] = value
        if record.exc_info:
            entry["error"] = self.formatException(record.exc_info)
        return json.dumps(entry, default=str)


def parse_level(name: str) -> int:
    """Accepts debug, info, warn or error, as LOG_LEVEL does in Go."""
    try:
        return _LEVELS[name.lower()]
    except KeyError:
        raise ValueError(f"invalid log level {name!r}") from None


def setup(
    service: str, level: int = logging.INFO, stream: TextIO = sys.stdout
) -> logging.Logger:
    """Sends every log record of the process to stream as one JSON object per
    line, with the service name and the current trace and span IDs, and
    returns the service's logger."""
    handler = logging.StreamHandler(stream)
    handler.setFormatter(_JSONFormatter(service))
    root = logging.getLogger()
    root.handlers[:] = [handler]
    root.setLevel(level)
    # pika logs every step of every connection attempt, errors included; the
    # consumer reports what matters once per attempt.
    logging.getLogger("pika").setLevel(logging.CRITICAL)
    return logging.getLogger(service)
