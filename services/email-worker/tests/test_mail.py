"""Sending through Mailpit, and how each failure is told apart (D69)."""

import socket
import ssl
import threading
import time
from dataclasses import dataclass, field
from datetime import UTC, datetime

import pytest
from labflow import containers

from email_worker import mail, render
from email_worker.mail import Mailer, MaybeSent, NotSent, Refused


@pytest.fixture(name="message")
def fixture_message(example):
    """A confirmation email."""
    return render.order_confirmation(
        "e1",
        datetime.now(UTC),
        example["data"],
        "https://shop.example/orders/o#token=t",
    )


def test_sends_over_starttls_with_a_login(mailer, inbox, message):
    """The email arrives, as sent."""
    mailer.send(message)
    [received] = inbox.messages()
    assert received["MessageID"] == "e1@labflow.example"
    assert [to["Address"] for to in received["To"]] == ["ada@example.com"]


@pytest.mark.parametrize(
    "failure",
    [
        ("recipient", 550, Refused),
        ("sender", 550, Refused),
        ("recipient", 451, NotSent),
        ("sender", 421, NotSent),
        ("authentication", 454, NotSent),
    ],
)
def test_failures(mailer, inbox, message, failure):
    """A 5xx reply after login refuses the email for good; a 4xx reply, or any
    refusal of the login, is an outage."""
    stage, code, want = failure
    inbox.chaos(**{stage: code})
    with pytest.raises(want) as raised:
        mailer.send(message)
    if want is Refused:
        assert raised.value.code == code
    assert not inbox.messages()


def test_a_refused_password_is_an_outage(mailpit_server, ca_file, inbox, message):
    """A rejected password, even with a 5xx reply, is ours to fix, so the
    email waits."""
    mailer = Mailer(mailpit_server.smtp_addr, "email-worker", "wrong" * 8, ca_file)
    with pytest.raises(NotSent, match="session refused with 535"):
        mailer.send(message)
    assert not inbox.messages()


def test_only_the_given_certificate_is_trusted(
    mailpit_server, ca_file, tmp_path, message, monkeypatch
):
    """Another certificate for the same name is not trusted, even when the
    system's trust store holds Mailpit's: the password is never sent to a
    server whose certificate is not the one given."""
    monkeypatch.setenv("SSL_CERT_FILE", ca_file)
    other = tmp_path / "other.crt"
    other.write_text(containers.self_signed("localhost")[0], encoding="utf-8")
    mailer = Mailer(
        mailpit_server.smtp_addr,
        "email-worker",
        mailpit_server.smtp_password,
        str(other),
    )
    with pytest.raises(NotSent, match="CERTIFICATE_VERIFY_FAILED"):
        mailer.send(message)


def test_no_server_is_an_outage(ca_file, message):
    """Nothing listening is an outage."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    mailer = Mailer(f"localhost:{port}", "email-worker", "x" * 40, ca_file)
    with pytest.raises(NotSent, match="connect"):
        mailer.send(message)


@dataclass
class Delays:
    """Seconds FakeSMTP waits: before each reply, between a reply's bytes,
    before answering the whole email, and before greeting its first client."""

    reply: float = 0
    trickle: float = 0
    answer: float = 0
    greeting: float = 0


@dataclass
class Behaviour:
    """How FakeSMTP misbehaves: it closes the connection instead of answering
    the recipient or the whole email (drop_at "rcpt" or "data"), or halfway
    through the email (drop_at "payload"), greets with another code, never
    answers one command (stall_at), answers the recipient with rcpt_code, and
    waits as delays say; it serves that many sessions, each on a thread of
    its own."""

    drop_at: str = ""
    stall_at: bytes = b""
    greeting: bytes = b"220 fake ESMTP"
    rcpt_code: int = 250
    sessions: int = 1
    delays: Delays = field(default_factory=Delays)


class FakeSMTP(threading.Thread):
    """An SMTP server speaking STARTTLS and AUTH PLAIN, as Mailpit does, that
    misbehaves as told."""

    def __init__(self, tls: tuple[str, str], tmp_path, behaviour: Behaviour):
        super().__init__(daemon=True)
        cert, key = tmp_path / "cert.pem", tmp_path / "key.pem"
        cert.write_text(tls[0], encoding="utf-8")
        key.write_text(tls[1], encoding="utf-8")
        self._tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self._tls.load_cert_chain(cert, key)
        self._listener = socket.create_server(("127.0.0.1", 0))
        self.addr = f"localhost:{self._listener.getsockname()[1]}"
        self._b = behaviour
        self.got_data = False

    def run(self) -> None:
        with self._listener:
            sessions = []
            for number in range(self._b.sessions):
                conn, _ = self._listener.accept()
                delay = self._b.delays.greeting if number == 0 else 0
                sessions.append(
                    threading.Thread(target=self._serve, args=(conn, delay))
                )
                sessions[-1].start()
        for session in sessions:
            session.join()

    def _serve(self, conn: socket.socket, greeting_delay: float) -> None:
        time.sleep(greeting_delay)
        try:
            self._session(conn)
        except OSError:
            pass  # the client gave up

    def _reply(self, stream, text: bytes) -> None:
        time.sleep(self._b.delays.reply)
        for i in range(len(text)):
            stream.write(text[i : i + 1])
            stream.flush()
            time.sleep(self._b.delays.trickle)
        stream.write(b"\r\n")
        stream.flush()

    def _session(self, conn: socket.socket) -> None:
        stream = conn.makefile("rwb")
        self._reply(stream, self._b.greeting)
        secured = False
        while line := stream.readline():
            command = line.split(b" ", 1)[0].upper().strip()
            if command == self._b.stall_at:
                while stream.readline():  # until the client gives up
                    pass
                break
            if command == b"STARTTLS":
                self._reply(stream, b"220 go ahead")
                conn = self._tls.wrap_socket(conn, server_side=True)
                stream, secured = conn.makefile("rwb"), True
                continue
            if (command, self._b.drop_at) == (b"RCPT", "rcpt"):
                break
            if command == b"DATA":
                self.got_data = True
                self._reply(stream, b"354 go ahead")
                if self._b.drop_at == "payload":
                    stream.readline()
                    break
                while stream.readline() != b".\r\n":
                    pass
                if self._b.drop_at == "data":
                    break
                time.sleep(self._b.delays.answer)
            replies = {
                b"EHLO": b"250-fake\r\n250 "
                + (b"AUTH PLAIN" if secured else b"STARTTLS"),
                b"AUTH": b"235 ok",
                b"MAIL": b"250 ok",
                b"RCPT": b"%d recipient" % self._b.rcpt_code,
                b"DATA": b"250 queued",
                b"RSET": b"250 ok",
            }
            self._reply(stream, replies.get(command, b"221 bye"))
        conn.close()


@pytest.fixture(name="fake_smtp")
def fixture_fake_smtp(tls, tmp_path):
    """Starts a FakeSMTP with Mailpit's certificate."""

    def start(
        delay: float = 0,
        trickle: float = 0,
        answer_delay: float = 0,
        greeting_delay: float = 0,
        **behaviour,
    ) -> FakeSMTP:
        delays = Delays(delay, trickle, answer_delay, greeting_delay)
        server = FakeSMTP(tls, tmp_path, Behaviour(**behaviour, delays=delays))
        server.start()
        return server

    return start


def send_to(server: FakeSMTP, ca_file: str, message) -> None:
    """Sends message to server as email-worker."""
    Mailer(server.addr, "email-worker", "x" * 40, ca_file).send(message)


@pytest.mark.parametrize("case", [("rcpt", NotSent), ("data", MaybeSent)])
def test_a_lost_connection_is_ambiguous_only_after_the_whole_email(
    fake_smtp, ca_file, message, case
):
    """A connection lost before the whole email was written cannot have
    delivered it; lost after, it may have."""
    drop_at, want = case
    server = fake_smtp(drop_at=drop_at)
    with pytest.raises(want):
        send_to(server, ca_file, message)
    server.join(5)


def test_a_slow_server_cannot_stretch_an_email_past_its_deadline(
    fake_smtp, ca_file, message, monkeypatch
):
    """A server that answers every command slowly, each within any per-step
    timeout, still fails the email at its one deadline, so a shutdown or
    another worker's claim is never outlasted."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 3)
    server = fake_smtp(delay=1)
    start = time.monotonic()
    with pytest.raises(NotSent):
        send_to(server, ca_file, message)
    assert time.monotonic() - start < 4.5, "the email outlived its deadline"


def test_a_trickling_reply_cannot_outlast_the_deadline(
    fake_smtp, ca_file, message, monkeypatch
):
    """One reply arriving a byte at a time, each in time for a per-read
    timeout, still ends at the email's deadline."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 2)
    server = fake_smtp(trickle=0.3)
    start = time.monotonic()
    with pytest.raises(NotSent):
        send_to(server, ca_file, message)
    assert time.monotonic() - start < 3, "the email outlived its deadline"


def test_a_stalled_name_lookup_cannot_outlast_the_deadline(
    ca_file, message, monkeypatch
):
    """A resolver that never answers fails the email at its deadline, and
    nothing is written if it answers later."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 1)
    resolved = threading.Event()

    def stalled(*_args, **_kwargs):
        resolved.wait(30)
        raise socket.gaierror("stalled")

    monkeypatch.setattr(socket, "getaddrinfo", stalled)
    start = time.monotonic()
    try:
        with pytest.raises(NotSent):
            Mailer("mail.example:25", "email-worker", "x" * 40, ca_file).send(message)
        assert time.monotonic() - start < 2, "the email outlived its deadline"
    finally:
        resolved.set()


def test_a_refusal_survives_a_server_that_stalls_after_it(
    fake_smtp, ca_file, message, monkeypatch
):
    """smtplib resets the session after a refused recipient; a server that
    never answers the reset cannot turn the refusal into an outage."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 2)
    server = fake_smtp(rcpt_code=550, stall_at=b"RSET")
    with pytest.raises(Refused) as raised:
        send_to(server, ca_file, message)
    assert raised.value.code == 550


def test_an_accepted_email_survives_a_server_that_stalls_on_quit(
    fake_smtp, ca_file, message, monkeypatch
):
    """Once the server accepted the email, a QUIT it never answers neither
    makes the email look unsent nor holds the worker."""
    monkeypatch.setattr(mail, "ANSWER_DEADLINE_SECONDS", 3)
    start = time.monotonic()
    send_to(fake_smtp(stall_at=b"QUIT"), ca_file, message)
    assert time.monotonic() - start < 2, "the worker waited on QUIT"


def test_mailpit_keeps_emails_7_days_at_most():
    """Mailpit's inbox holds customers' addresses and order links, so it
    keeps no email longer than an event may live (D64); the test Mailpit runs
    with the same setting."""
    environment = containers.compose_service("mailpit")["environment"]
    assert environment["MP_MAX_AGE"] == "7d"


def late_lookup(monkeypatch, delay: float):
    """Makes the first name lookup answer, correctly, after delay seconds."""
    real, lookups = socket.getaddrinfo, []

    def late(_host, *args, **kwargs):
        lookups.append(1)
        if len(lookups) == 1:
            time.sleep(delay)
        return real("127.0.0.1", *args, **kwargs)

    monkeypatch.setattr(socket, "getaddrinfo", late)


def test_a_late_name_lookup_writes_nothing(fake_smtp, ca_file, message, monkeypatch):
    """A resolver that answers after the deadline lets the abandoned session
    write nothing: the email reported as not sent is never handed over behind
    the worker's back, and no new session starts behind the stuck one."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 1)
    server = fake_smtp()
    late_lookup(monkeypatch, 1.5)
    mailer = Mailer(server.addr, "email-worker", "x" * 40, ca_file)
    with pytest.raises(NotSent):
        mailer.send(message)
    with pytest.raises(NotSent, match="has not ended"):
        mailer.send(message)
    server.join(6)
    assert not server.got_data, "the email was handed over after NotSent"


def test_a_late_name_lookup_does_not_hold_the_next_send(
    fake_smtp, ca_file, message, monkeypatch
):
    """A lookup that answers late does not connect, even to a server slow to
    greet, so the next send starts as soon as the lookup has answered."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 1)
    server = fake_smtp(greeting_delay=5, sessions=2)
    late_lookup(monkeypatch, 1.5)
    mailer = Mailer(server.addr, "email-worker", "x" * 40, ca_file)
    with pytest.raises(NotSent):
        mailer.send(message)
    time.sleep(1.5)  # the late lookup has answered
    mailer.send(message)


def test_a_refused_greeting_is_an_outage(fake_smtp, ca_file, message):
    """A server that greets with anything but 220 is not taking email."""
    with pytest.raises(NotSent, match="greeting answered 421"):
        send_to(fake_smtp(greeting=b"421 too busy"), ca_file, message)


def test_a_connection_lost_halfway_through_the_email_may_have_sent_it(
    fake_smtp, ca_file, message
):
    """Once the email started being handed over, a lost connection leaves it
    possibly accepted."""
    with pytest.raises(MaybeSent):
        send_to(fake_smtp(drop_at="payload"), ca_file, message)


def test_a_slow_answer_after_the_handover_is_awaited(
    fake_smtp, ca_file, message, monkeypatch
):
    """A server that takes long to answer the whole email, scanning it, say,
    gets its own deadline for the answer: the email is accepted once, not
    abandoned and sent again after every claim."""
    monkeypatch.setattr(mail, "SEND_DEADLINE_SECONDS", 1)
    monkeypatch.setattr(mail, "ANSWER_DEADLINE_SECONDS", 5)
    send_to(fake_smtp(answer_delay=2), ca_file, message)


def test_no_answer_within_its_deadline_may_have_sent_the_email(
    fake_smtp, ca_file, message, monkeypatch
):
    """Past the answer's deadline the email may have been accepted."""
    monkeypatch.setattr(mail, "ANSWER_DEADLINE_SECONDS", 1)
    start = time.monotonic()
    with pytest.raises(MaybeSent):
        send_to(fake_smtp(answer_delay=4), ca_file, message)
    assert time.monotonic() - start < 3, "the answer outlived its deadline"
