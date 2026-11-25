"""Sending through the mail server, Mailpit in the lab, a stand-in for an
email provider (D69).

Every failure says whether the email could have been accepted, which decides
what happens to the send's claim:

- NotSent: no connection, STARTTLS or login, or a 4xx reply: an outage, and
  the email was certainly not accepted;
- MaybeSent: the email was being handed over, or had been, when the
  connection broke or the deadline passed, so it may have been accepted;
- Refused: a 5xx reply after login refused this email for good.
"""

import smtplib
import socket
import ssl
import threading
import time
from contextlib import suppress
from dataclasses import dataclass, field
from email.message import EmailMessage

from labflow.config import split_url
from labflow.errors import capture
from labflow.rabbitmq import Outage

# Two deadlines for the whole session, name resolution included, since
# smtplib's own timeout bounds each read and write, not the session: one until
# the email starts being handed over, a failure before which is certainly no
# send, and one for the server's answer after it, long enough for a server
# that scans what it receives. Together below the 2-minute claim (D69) and
# what a graceful stop leaves a message in hand.
SEND_DEADLINE_SECONDS = 10.0
ANSWER_DEADLINE_SECONDS = 20.0


class _SMTP(smtplib.SMTP):
    """smtplib's client, noting when the email starts being handed over, from
    which point it may be accepted and its deadline is the answer's, and
    refusing to connect, read or write once its caller has given up on it."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._abandoned = False
        self.handing_over = False
        self.deadline = time.monotonic() + SEND_DEADLINE_SECONDS
        super().__init__(local_hostname="email-worker", timeout=SEND_DEADLINE_SECONDS)

    def _check(self) -> None:
        if self._abandoned:
            raise TimeoutError("the email's deadline passed")

    def _get_socket(self, host: str, port: int, timeout: float) -> socket.socket:
        # A lookup that ends after the caller gave up must not connect.
        sock = super()._get_socket(host, port, timeout)
        with self._lock:
            if self._abandoned:
                sock.close()
                self._check()
        return sock

    def getreply(self) -> tuple[int, bytes]:
        with self._lock:
            self._check()
        return super().getreply()

    def send(self, s: bytes | str) -> None:
        with self._lock:
            self._check()
            # DATA's payload ends with the lone dot: from its first byte, the
            # server may end up accepting the email.
            if isinstance(s, bytes) and s.endswith(b"\r\n.\r\n"):
                self.handing_over = True
                self.deadline = time.monotonic() + ANSWER_DEADLINE_SECONDS
                if self.sock is not None:
                    self.sock.settimeout(ANSWER_DEADLINE_SECONDS)
        super().send(s)

    def rset(self) -> tuple[int, bytes]:
        """smtplib resets the session after a refusal before raising it; the
        session ends right after, so the reset is skipped, and a server that
        stalls on it cannot hide the refusal."""
        return 250, b""

    def abandon(self) -> bool:
        """Stops any further connection, read or write, and wakes a blocked
        one; returns whether the email had started being handed over."""
        with self._lock:
            self._abandoned = True
            handing_over = self.handing_over
        if self.sock is not None:
            with suppress(OSError):
                self.sock.shutdown(socket.SHUT_RDWR)
        return handing_over


class NotSent(Outage):
    """The mail server is unavailable; the email was not accepted."""


class MaybeSent(Outage):
    """The email may have been accepted, but no answer came."""


class Refused(Exception):
    """The mail server refused this email for good, at a stage: "recipient",
    a fault of the address; "sender" or "message", not."""

    def __init__(self, code: int, stage: str):
        super().__init__(f"{stage} refused with {code}")
        self.code, self.stage = code, stage


def _refusal(code: int, stage: str) -> Exception:
    if 500 <= code <= 599:
        return Refused(code, stage)
    return NotSent(f"{stage} answered {code}")


def smtp_address(addr: str) -> tuple[str, int] | None:
    """The host and port of host:port, or None unless that is all it holds,
    with a valid port: no user, path, query or fragment."""
    u = split_url(f"smtp://{addr}", ("smtp",))
    if u is None or u.port is None or "@" in addr or u.netloc != addr:
        return None
    return u.hostname, u.port


@dataclass
class Mailer:
    """Sends one email per connection over STARTTLS, which it requires,
    trusting only the certificates in ca_file, and logs in before sending."""

    addr: str
    user: str
    password: str = field(repr=False)
    ca_file: str

    def __post_init__(self) -> None:
        address = smtp_address(self.addr)
        if address is None:
            raise ValueError("addr must be host:port")
        self._address = address
        self._tls = ssl.create_default_context(cafile=self.ca_file)  # no system CAs
        self._lingering: threading.Thread | None = None

    def send(self, message: EmailMessage) -> None:
        """Hands message over within SEND_DEADLINE_SECONDS, and waits up to
        ANSWER_DEADLINE_SECONDS more for the answer, raising NotSent,
        MaybeSent or Refused. The session runs on a thread of its own, which
        is abandoned at its deadline: a stalled name lookup cannot be
        interrupted, but nothing is connected, read or written after it."""
        # A session abandoned at its deadline that has not ended holds a
        # stalled resolver or connection: no other starts behind it.
        if self._lingering is not None and self._lingering.is_alive():
            raise NotSent("an abandoned session has not ended")
        smtp, outcome = _SMTP(), []

        def run() -> None:
            outcome.append(capture(self._session, smtp, message))
            # The outcome is known: a server that stalls on QUIT cannot
            # change it.
            try:
                smtp.quit()
            except (OSError, smtplib.SMTPException):
                smtp.close()

        session = threading.Thread(target=run, daemon=True)
        session.start()
        # Until the outcome is known: QUIT, after it, is only cleanup.
        while not outcome and (left := smtp.deadline - time.monotonic()) > 0:
            session.join(min(left, 0.1))
        if session.is_alive():
            handing_over = smtp.abandon()
            self._lingering = session
            if not outcome:
                if handing_over:
                    raise MaybeSent("no answer to the email within its deadline")
                raise NotSent("the mail server did not answer within the deadline")
        if outcome[0] is not None:
            raise outcome[0]

    def _session(self, smtp: _SMTP, message: EmailMessage) -> None:
        try:
            code, _ = smtp.connect(*self._address)
        except (OSError, smtplib.SMTPException) as e:
            raise NotSent(f"connect: {type(e).__name__}") from None
        if code != 220:
            raise NotSent(f"greeting answered {code}")
        self._send(smtp, message)

    def _send(self, smtp: _SMTP, message: EmailMessage) -> None:
        try:
            smtp.ehlo()
            smtp.starttls(context=self._tls)
            smtp.ehlo()
            smtp.login(self.user, self.password)
        except smtplib.SMTPResponseException as e:
            # Even a 5xx here, such as 535 for a refused password, is ours to
            # fix, not the email's.
            raise NotSent(f"session refused with {e.smtp_code}") from None
        except (OSError, smtplib.SMTPException) as e:
            raise NotSent(f"session: {e!r}") from None
        try:
            smtp.send_message(message)
        except smtplib.SMTPRecipientsRefused as e:
            code, _ = next(iter(e.recipients.values()))
            raise _refusal(code, "recipient") from None
        except smtplib.SMTPSenderRefused as e:
            raise _refusal(e.smtp_code, "sender") from None
        except smtplib.SMTPDataError as e:
            raise _refusal(e.smtp_code, "message") from None
        except (OSError, smtplib.SMTPException) as e:
            if smtp.handing_over:
                raise MaybeSent(f"no answer to the email: {type(e).__name__}") from None
            raise NotSent(
                f"before the email was handed over: {type(e).__name__}"
            ) from None
