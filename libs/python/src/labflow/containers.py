"""Throwaway containers for integration tests, configured as Docker Compose
configures the real ones: the images compose.yaml names, RabbitMQ loaded with
its definitions template, a random password for every user, and ports
published on 127.0.0.1 only (D40). Each container carries the label
labflow.test with the test process's ID, so `make test` removes what a killed
run left behind. Import it only from tests: it needs the development
requirements.

The Python counterpart of libs/go/pgtest, rabbitmqtest and dockertest.
"""

import datetime
import json
import os
import secrets
import smtplib
import string
import tempfile
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager, suppress
from pathlib import Path
from typing import Any

import pika
import psycopg
import yaml
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID
from psycopg.conninfo import make_conninfo
from testcontainers.core.config import testcontainers_config
from testcontainers.core.container import DockerContainer
from testcontainers.core.exceptions import ContainerStartException

# The reaper publishes a port on every interface while holding the Docker
# socket; leftovers are removed by `make test` instead.
testcontainers_config.ryuk_disabled = True

ROOT = Path(__file__).resolve().parents[4]
VHOST = "labflow"
_LOCALHOST = "127.0.0.1"
_STARTUP_SECONDS = 90


def password() -> str:
    """52 random letters and digits, as local-secrets generates."""
    alphabet = string.ascii_letters + string.digits
    return "".join(secrets.choice(alphabet) for _ in range(52))


def compose_service(name: str) -> dict:
    """A service's settings in Docker Compose."""
    with open(ROOT / "compose.yaml", encoding="utf-8") as f:
        return yaml.safe_load(f)["services"][name]


class Redacted(str):
    """A string holding a credential, such as a connection string with its
    password, that a failing test's report shows as <redacted>: pytest prints
    the arguments of every function in a traceback."""

    def __repr__(self) -> str:
        return "'<redacted>'"


class RedactedBytes(bytes):
    """Bytes holding a secret, such as a signing key, that a failing test's
    report shows as <redacted>."""

    def __repr__(self) -> str:
        return "b'<redacted>'"


def secret_file(directory: str, name: str, content: str) -> str:
    """Writes a file the container's own user can read, and returns its path."""
    path = os.path.join(directory, name)
    with open(path, "w", encoding="utf-8") as f:
        f.write(content)
    os.chmod(path, 0o644)
    return path


def until[T](probe: Callable[[], T], what: str) -> T:
    """Calls probe until it returns without raising, for at most 90 seconds."""
    deadline = time.monotonic() + _STARTUP_SECONDS
    while True:
        try:
            return probe()
        except Exception:  # noqa: BLE001  # pylint: disable=broad-exception-caught
            if time.monotonic() > deadline:
                raise TimeoutError(f"{what} did not start") from None
            time.sleep(0.5)


@contextmanager
def container(image: str, *ports: int, **options: Any) -> Iterator[DockerContainer]:
    """A container whose ports are published on 127.0.0.1 only, created with
    the given Docker options; the caller configures and starts it. Removed
    with its volumes on exit."""
    c = DockerContainer(image).with_kwargs(
        labels={"labflow.test": str(os.getpid())}, **options
    )
    for port in ports:
        # docker-py takes (address, None) as a random port on that address;
        # with_bind_ports takes only a port.
        c.ports[str(port)] = (_LOCALHOST, None)
    try:
        yield c
    finally:
        with suppress(ContainerStartException):  # raised when it never started
            c.stop()


def address(c: DockerContainer, port: int) -> tuple[str, int]:
    """Where a published port answers on this host."""
    return _LOCALHOST, int(c.get_exposed_port(port))


def self_signed(host: str) -> tuple[str, str]:
    """A self-signed certificate for host and its key, in PEM, as
    local-secrets makes Mailpit's."""
    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, host)])
    now = datetime.datetime.now(datetime.UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(hours=1))
        .not_valid_after(now + datetime.timedelta(days=1))
        .add_extension(
            x509.SubjectAlternativeName([x509.DNSName(host)]), critical=False
        )
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
        .add_extension(
            x509.ExtendedKeyUsage([x509.oid.ExtendedKeyUsageOID.SERVER_AUTH]),
            critical=False,
        )
        .sign(key, hashes.SHA256())
    )
    return (
        cert.public_bytes(serialization.Encoding.PEM).decode(),
        Redacted(
            key.private_bytes(
                serialization.Encoding.PEM,
                serialization.PrivateFormat.PKCS8,
                serialization.NoEncryption(),
            ).decode()
        ),
    )


class Postgres:
    """A PostgreSQL server whose superuser has a random password, passed as a
    file, so Docker's metadata never shows it."""

    def __init__(self, c: DockerContainer, superuser_password: str):
        self._container = c
        self._password = superuser_password

    def new_database(self) -> Redacted:
        """Creates an empty database and returns a connection string with its
        password."""
        name = "test_" + secrets.token_hex(8)
        with psycopg.connect(self.conninfo("postgres"), autocommit=True) as conn:
            conn.execute(f'CREATE DATABASE "{name}"')
        return self.conninfo(name)

    def conninfo(self, dbname: str) -> Redacted:
        """A connection string for a database, as the superuser."""
        host, port = address(self._container, 5432)
        return Redacted(
            make_conninfo(
                host=host,
                port=port,
                user="postgres",
                password=self._password,
                dbname=dbname,
            )
        )

    def pause(self) -> None:
        """Freezes the server, as with one that hangs."""
        self._container.get_wrapped_container().pause()

    def unpause(self) -> None:
        """Lets a paused server run again."""
        self._container.get_wrapped_container().unpause()


@contextmanager
def postgres() -> Iterator[Postgres]:
    """Starts the PostgreSQL image Docker Compose runs."""
    with (
        tempfile.TemporaryDirectory() as directory,
        container(compose_service("postgres")["image"], 5432) as c,
    ):
        superuser = password()
        path = secret_file(directory, "superuser", superuser)
        c.with_volume_mapping(path, "/run/secrets/superuser", "ro")
        c.with_env("POSTGRES_PASSWORD_FILE", "/run/secrets/superuser")
        c.start()
        server = Postgres(c, superuser)
        until(
            lambda: psycopg.connect(server.conninfo("postgres")).close(), "PostgreSQL"
        )
        yield server


def apply_migrations(conninfo: str, directory: Path) -> None:
    """Applies the Up section of every goose SQL file in directory, in name
    order, as the migrate command does."""
    with psycopg.connect(conninfo, autocommit=True) as conn:
        for path in sorted(directory.glob("*.sql")):
            text = path.read_text(encoding="utf-8")
            up = text.split("-- +goose Up", 1)[1].split("-- +goose Down", 1)[0]
            conn.execute(up)


class RabbitMQ:
    """A broker loaded with Docker Compose's definitions template, each user
    with a random password."""

    def __init__(self, c: DockerContainer, passwords: dict[str, str]):
        self._container = c
        self._passwords = passwords

    def url(self, user: str) -> Redacted:
        """The AMQP URL of a user from the definitions, with its password."""
        host, port = address(self._container, 5672)
        return Redacted(f"amqp://{user}:{self._passwords[user]}@{host}:{port}/{VHOST}")

    def close_connections(self) -> None:
        """Closes every client connection, as a broker restart would."""
        result = self._container.exec(
            ["rabbitmqctl", "close_all_connections", "--vhost", VHOST, "test"]
        )
        assert result.exit_code == 0, result.output

    def admin(self) -> pika.Connection:
        """A connection as the administrator, for setting up and inspecting."""
        return pika.Connection(pika.URLParameters(self.url("admin")))


def render_definitions(template: str, passwords: dict[str, str]) -> str:
    """Fills each user's "@name@" placeholder with its password, as
    rabbitmq.RenderDefinitions does."""
    for user, secret in passwords.items():
        template = template.replace(f'"@{user}@"', json.dumps(secret))
    return template


@contextmanager
def rabbitmq() -> Iterator[RabbitMQ]:
    """Starts the RabbitMQ image Docker Compose runs, with its definitions."""
    template = (ROOT / "compose/rabbitmq/definitions.template.json").read_text(
        encoding="utf-8"
    )
    users = [u["name"] for u in json.loads(template)["users"]]
    passwords = {user: password() for user in users}
    with (
        tempfile.TemporaryDirectory() as directory,
        container(compose_service("rabbitmq")["image"], 5672) as c,
    ):
        definitions = secret_file(
            directory, "definitions.json", render_definitions(template, passwords)
        )
        c.with_volume_mapping(
            definitions, "/run/secrets/rabbitmq-definitions.json", "ro"
        )
        c.with_volume_mapping(
            str(ROOT / "compose/rabbitmq/labflow.conf"),
            "/etc/rabbitmq/conf.d/90-labflow.conf",
            "ro",
        )
        c.start()
        broker = RabbitMQ(c, passwords)
        until(lambda: broker.admin().close(), "RabbitMQ")
        yield broker


class Mailpit:
    """Mailpit as Docker Compose runs it: SMTP login over required STARTTLS,
    here with a certificate for localhost, Chaos, and a login for its API."""

    def __init__(self, c: DockerContainer, smtp_password: str, ui_password: str):
        self._container = c
        self.smtp_password = Redacted(smtp_password)
        self.ui_auth = ("admin", Redacted(ui_password))

    @property
    def smtp_addr(self) -> str:
        """host:port of the SMTP server, by the name its certificate has."""
        return f"localhost:{address(self._container, 1025)[1]}"

    @property
    def api(self) -> str:
        """The base URL of the HTTP API."""
        host, port = address(self._container, 8025)
        return f"http://{host}:{port}/api/v1"


@contextmanager
def mailpit(certificate: str, key: str) -> Iterator[Mailpit]:
    """Starts Mailpit with Docker Compose's image, user and settings, and the
    PEM certificate and key given."""
    service = compose_service("mailpit")
    smtp_password, ui_password = password(), password()
    files = {
        "mailpit-smtp-users": f"email-worker:{smtp_password}\n",
        "mailpit-ui-users": f"admin:{ui_password}\n",
        "mailpit-tls.crt": certificate,
        "mailpit-tls.key": key,
    }
    with (
        tempfile.TemporaryDirectory() as directory,
        container(service["image"], 1025, 8025, user=service["user"]) as c,
    ):
        for name, content in files.items():
            path = secret_file(directory, name, content)
            c.with_volume_mapping(path, f"/run/secrets/{name}", "ro")
        for path in service["tmpfs"]:
            c.with_tmpfs_mount(path)
        c.with_envs(**service["environment"])
        c.start()
        server = Mailpit(c, smtp_password, ui_password)
        until(lambda: smtplib.SMTP(*address(c, 1025), timeout=2).quit(), "Mailpit")
        yield server
