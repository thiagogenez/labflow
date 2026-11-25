"""The client-side deadline on database calls, against a real PostgreSQL."""

import socket
import threading
import time

import psycopg
import pytest

from labflow import containers, postgres


@pytest.fixture(scope="module", name="server")
def fixture_server():
    """One PostgreSQL for the module's tests."""
    with containers.postgres() as server:
        yield server


def test_a_call_to_a_frozen_server_fails_at_its_deadline(server, monkeypatch):
    """A server that stops answering after the connection opened cannot fire
    its own timeouts; the deadline fails the call instead of waiting
    forever."""
    monkeypatch.setattr(postgres, "CALL_DEADLINE_SECONDS", 2)
    conn = postgres.connect(server.new_database())
    outcome = []

    def call() -> None:
        try:
            with postgres.deadline(conn):
                conn.execute("SELECT 1")
        except psycopg.OperationalError as e:
            outcome.append(e)

    caller = threading.Thread(target=call, daemon=True)
    server.pause()
    try:
        caller.start()
        caller.join(8)
    finally:
        server.unpause()
        conn.close()
    assert not caller.is_alive(), "the call outlived its deadline"
    assert outcome, "the call did not fail"


def test_a_call_within_its_deadline_keeps_the_connection(server, monkeypatch):
    """A call that ends in time cancels the deadline, so the connection stays
    usable afterwards."""
    monkeypatch.setattr(postgres, "CALL_DEADLINE_SECONDS", 1)
    with postgres.connect(server.new_database()) as conn:
        with postgres.deadline(conn):
            conn.execute("SELECT 1")
        time.sleep(1.5)
        assert conn.execute("SELECT 1").fetchone() == (1,)


def test_connection_strings_are_redacted_in_reports(server):
    """A failing test's report shows a connection string as <redacted>, never
    its password."""
    conninfo = server.new_database()
    assert isinstance(conninfo, containers.Redacted)
    assert "password" in conninfo
    assert repr(conninfo) == "'<redacted>'", "a report would show the password"


def test_a_stalled_name_lookup_fails_the_connection_at_its_deadline(monkeypatch):
    """psycopg resolves the host before its own connect timeout starts; a
    resolver that never answers fails the connection at the deadline."""
    monkeypatch.setattr(postgres, "CALL_DEADLINE_SECONDS", 1)
    resolved = threading.Event()

    def stalled(*_args, **_kwargs):
        resolved.wait(30)
        raise socket.gaierror("stalled")

    monkeypatch.setattr(socket, "getaddrinfo", stalled)
    start = time.monotonic()
    try:
        with pytest.raises(psycopg.OperationalError):
            postgres.connect("host=db.example user=x password=y dbname=z")
        assert time.monotonic() - start < 3, "the connection outlived its deadline"
        # While that attempt is stuck, another fails at once instead of
        # starting another thread behind it.
        start = time.monotonic()
        with pytest.raises(psycopg.OperationalError, match="has not ended"):
            postgres.connect("host=db.example user=x password=y dbname=z")
        assert time.monotonic() - start < 0.5, "a second attempt piled up"
    finally:
        resolved.set()
