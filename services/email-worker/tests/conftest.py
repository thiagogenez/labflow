"""Fixtures for email-worker's tests: a real PostgreSQL with the service's
migrations, and Mailpit as Docker Compose runs it, with a certificate made
for the test."""

import base64
import json
import secrets
import urllib.request
from pathlib import Path

import pytest
from labflow import containers
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from email_worker.mail import Mailer
from email_worker.store import Store

MIGRATIONS = Path(__file__).resolve().parents[1] / "migrations" / "email"
EXAMPLE = (
    Path(__file__).resolve().parents[3]
    / "contracts/events/examples/labflow.order.placed.v1.json"
)
_SPANS = InMemorySpanExporter()


@pytest.fixture(scope="session", autouse=True)
def fixture_tracer_provider():
    """Spans go to memory, so tests can read what they record."""
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(_SPANS))
    trace.set_tracer_provider(provider)


@pytest.fixture(name="spans")
def fixture_spans():
    """The spans finished during the test."""
    _SPANS.clear()
    return _SPANS


@pytest.fixture(name="example")
def fixture_example():
    """The contract's order.placed example, parsed."""
    return json.loads(EXAMPLE.read_bytes())


@pytest.fixture(scope="session", name="tls")
def fixture_tls():
    """Mailpit's certificate and key, for localhost."""
    return containers.self_signed("localhost")


@pytest.fixture(scope="session", name="mailpit_server")
def fixture_mailpit_server(tls):
    """One Mailpit for the session."""
    with containers.mailpit(*tls) as server:
        yield server


class Inbox:
    """Mailpit's API, as a logged-in user."""

    def __init__(self, server: containers.Mailpit):
        self._server = server
        user, password = server.ui_auth
        token = base64.b64encode(f"{user}:{password}".encode()).decode()
        self._auth = {"Authorization": f"Basic {token}"}

    def _call(self, method: str, path: str, body: dict | None = None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(
            self._server.api + path,
            data=data,
            method=method,
            headers=self._auth | {"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            json_reply = response.headers.get_content_type() == "application/json"
            content = response.read()
        return json.loads(content) if json_reply else None

    def messages(self) -> list[dict]:
        """Every email received, newest first."""
        return self._call("GET", "/messages")["messages"]

    def text(self, message: dict) -> str:
        """An email's text part."""
        return self._call("GET", f"/message/{message['ID']}")["Text"]

    def clear(self) -> None:
        """Deletes every email."""
        self._call("DELETE", "/messages", {})

    def chaos(self, **triggers: int) -> None:
        """Makes Mailpit answer a stage with a code, such as recipient=550;
        no arguments restore normal answers."""
        self._call(
            "PUT",
            "/chaos",
            {
                stage.capitalize(): {"ErrorCode": code, "Probability": 100}
                for stage, code in triggers.items()
            },
        )


@pytest.fixture(name="inbox")
def fixture_inbox(mailpit_server):
    """An empty inbox, with Chaos off before and after the test."""
    inbox = Inbox(mailpit_server)
    inbox.chaos()
    inbox.clear()
    yield inbox
    inbox.chaos()


@pytest.fixture(name="ca_file")
def fixture_ca_file(tls, tmp_path):
    """The file email-worker trusts: Mailpit's certificate alone."""
    path = tmp_path / "mailpit.crt"
    path.write_text(tls[0], encoding="utf-8")
    return str(path)


@pytest.fixture(name="mailer")
def fixture_mailer(mailpit_server, ca_file):
    """A mailer logged in as email-worker."""
    return Mailer(
        mailpit_server.smtp_addr, "email-worker", mailpit_server.smtp_password, ca_file
    )


@pytest.fixture(scope="session", name="postgres_server")
def fixture_postgres_server():
    """One PostgreSQL for the session."""
    with containers.postgres() as server:
        yield server


@pytest.fixture(name="conninfo")
def fixture_conninfo(postgres_server):
    """A new database with email-worker's migrations applied."""
    conninfo = postgres_server.new_database()
    containers.apply_migrations(conninfo, MIGRATIONS)
    return conninfo


@pytest.fixture(name="store")
def fixture_store(conninfo):
    """The worker's store on a fresh database."""
    store = Store(conninfo)
    yield store
    store.close()


@pytest.fixture(name="token_key")
def fixture_token_key():
    """A random order-token key, as local-secrets generates."""
    return containers.RedactedBytes(secrets.token_bytes(32))
