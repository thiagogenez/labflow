"""OpenTelemetry tracing, configured as libs/go/telemetry configures it."""

import os
import threading
from collections.abc import Callable

from opentelemetry import propagate, trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import SERVICE_NAME, SERVICE_VERSION, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from labflow.config import ConfigError

_FLUSH_TIMEOUT_SECONDS = 5.0


def setup(service: str, version: str) -> Callable[[], None]:
    """Installs the global tracer provider and the W3C Trace Context
    propagator. The exporter follows the standard OTEL_* variables:
    OTEL_TRACES_EXPORTER is otlp (the default, over HTTP) or none, which keeps
    trace IDs for log correlation without exporting spans. Returns a function
    that flushes pending spans, waiting at most five seconds."""
    exporter = os.environ.get("OTEL_TRACES_EXPORTER", "otlp").strip() or "otlp"
    protocol = os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf").strip()
    if exporter not in ("otlp", "none"):
        raise ConfigError(
            f"OTEL_TRACES_EXPORTER must be otlp or none, got {exporter!r}"
        )
    if exporter == "otlp" and protocol != "http/protobuf":
        raise ConfigError(
            f"OTEL_EXPORTER_OTLP_PROTOCOL must be http/protobuf, got {protocol!r}"
        )
    # Resource.create adds OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME.
    # shutdown_on_exit=False: the SDK's exit hook would wait on the exporter
    # again, without flush's limit.
    provider = TracerProvider(
        resource=Resource.create({SERVICE_NAME: service, SERVICE_VERSION: version}),
        shutdown_on_exit=False,
    )
    if exporter == "otlp":
        provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(provider)
    propagate.set_global_textmap(TraceContextTextMapPropagator())

    def flush() -> None:
        # Best effort within five seconds: the SDK's own timeouts do not bound
        # an export to a collector that stopped answering, and losing the last
        # spans must not hold shutdown past the grace period.
        def export_and_stop() -> None:
            provider.force_flush()
            provider.shutdown()

        worker = threading.Thread(target=export_and_stop, daemon=True)
        worker.start()
        worker.join(_FLUSH_TIMEOUT_SECONDS)

    return flush
