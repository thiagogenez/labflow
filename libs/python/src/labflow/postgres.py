"""PostgreSQL connections, configured as libs/go/postgres configures them."""

import os
import socket
import threading
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from urllib.parse import parse_qsl

import psycopg
from psycopg.conninfo import make_conninfo

from labflow.config import Env, split_url

# Every statement and lock wait is bounded by the server, and every call by
# the client, so a database that stops answering fails the work in hand
# instead of holding it.
CONNECT_TIMEOUT_SECONDS = 5
# Above the server's 5-second statement timeout, below what a graceful stop
# leaves a message in hand.
CALL_DEADLINE_SECONDS = 6.0
_OPTIONS = "-c statement_timeout=5s -c lock_timeout=2s"
# Connection attempts abandoned at their deadline that have not ended: while
# one does, a stalled resolver holds it, and further attempts fail at once
# instead of piling up threads behind it.
_ABANDONED: list[threading.Thread] = []
_ABANDONED_LOCK = threading.Lock()

# libpq takes credentials from many query options (password, passfile,
# sslpassword, oauth_client_secret, ...), so only these are accepted.
QUERY_OPTIONS = ("application_name", "sslmode", "sslrootcert")


def dsn_from_env(env: Env, name: str) -> str:
    """Reads a connection URL without its password from the variable name and
    the password from its secret: DATABASE_URL takes its password from the
    file DATABASE_PASSWORD_FILE names. A URL that carries a password, or a
    query option other than QUERY_OPTIONS, is refused, and no error repeats
    either value."""
    raw = env.required(name)
    password = env.secret(name.removesuffix("_URL") + "_PASSWORD")
    if not raw:
        return ""
    u = split_url(raw, ("postgres", "postgresql"))
    if u is None or not u.username:
        env.check(
            False,
            f"{name} must be a postgres:// URL with a host, a user and a valid port",
        )
        return ""
    if u.password is not None or u.fragment:
        env.check(False, f"{name} must not carry a password; its secret file holds it")
        return ""
    try:
        pairs = parse_qsl(u.query, keep_blank_values=True, strict_parsing=bool(u.query))
    except ValueError:
        pairs = [("", "")]  # unparsable: refused below
    # Compared as libpq reads them: a key padded with spaces is no option it
    # knows, and is refused here rather than by the driver.
    if not {key for key, _ in pairs} <= set(QUERY_OPTIONS):
        env.check(
            False, f"{name} may set only the query options {', '.join(QUERY_OPTIONS)}"
        )
        return ""
    try:
        return make_conninfo(raw, password=password)
    except (psycopg.ProgrammingError, ValueError):  # ValueError: bad % escapes
        env.check(False, f"{name} is not a connection string libpq accepts")
        return ""


def connect(dsn: str) -> psycopg.Connection:
    """Opens a connection in autocommit mode with bounded statements and lock
    waits, within CALL_DEADLINE_SECONDS: psycopg resolves the host name before
    its own connect timeout starts, and a stalled resolver would hold the
    caller. A connection that opens after the deadline is closed."""
    with _ABANDONED_LOCK:
        _ABANDONED[:] = [t for t in _ABANDONED if t.is_alive()]
        if _ABANDONED:
            raise psycopg.OperationalError(
                "an abandoned connection attempt has not ended"
            )
    lock, outcome = threading.Lock(), {}

    def open_connection() -> None:
        try:
            conn = psycopg.connect(
                dsn,
                autocommit=True,
                connect_timeout=CONNECT_TIMEOUT_SECONDS,
                options=_OPTIONS,
            )
        except psycopg.Error as e:
            outcome["error"] = e
            return
        with lock:
            if "abandoned" in outcome:
                conn.close()
            else:
                outcome["conn"] = conn

    opening = threading.Thread(target=open_connection, daemon=True)
    opening.start()
    opening.join(CALL_DEADLINE_SECONDS)
    with lock:
        if "conn" in outcome:
            return outcome["conn"]
        if "error" in outcome:
            raise outcome["error"]
        outcome["abandoned"] = True
    with _ABANDONED_LOCK:
        _ABANDONED.append(opening)
    raise psycopg.OperationalError("the connection did not open within its deadline")


@contextmanager
def deadline(conn: psycopg.Connection) -> Iterator[None]:
    """Shuts the connection's socket down if the block runs longer than
    CALL_DEADLINE_SECONDS. The server's own timeouts
    cannot fire while it is frozen or unreachable, and a socket that stays
    open waits for a reply forever; shut, it fails the call at once, and the
    connection must be discarded."""

    def cut() -> None:
        with suppress(OSError), socket.socket(fileno=os.dup(conn.fileno())) as s:
            s.shutdown(socket.SHUT_RDWR)

    timer = threading.Timer(CALL_DEADLINE_SECONDS, cut)
    timer.daemon = True
    timer.start()
    try:
        yield
    finally:
        timer.cancel()
