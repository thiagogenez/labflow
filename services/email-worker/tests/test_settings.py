"""email-worker's configuration: every problem reported at startup, and no
secret from a variable's value."""

import base64
import secrets

import pytest
from labflow.config import ConfigError, Env
from labflow.containers import Redacted

from email_worker.__main__ import Settings, readiness


@pytest.fixture(name="files")
def fixture_files(tmp_path):
    """Secret files with random values, as local-secrets writes them."""
    # Redacted, so a failing test's report never shows them.
    files = {
        "db": Redacted(secrets.token_hex(20)),
        "rabbit": Redacted(secrets.token_hex(20)),
        "smtp": Redacted(secrets.token_hex(20)),
        "key": Redacted(base64.b64encode(secrets.token_bytes(32)).decode()),
    }
    for name, content in files.items():
        (tmp_path / name).write_text(content + "\n", encoding="utf-8")
    return files


@pytest.fixture(name="environ")
def fixture_environ(tmp_path, ca_file, files):
    """A complete, valid environment."""
    path = {name: str(tmp_path / name) for name in files}
    return {
        "DATABASE_URL": "postgres://email_app@postgres:5432/email",
        "DATABASE_PASSWORD_FILE": path["db"],
        "BROKER": "rabbitmq",
        "RABBITMQ_URL": "amqp://email-worker@rabbitmq:5672/labflow",
        "RABBITMQ_PASSWORD_FILE": path["rabbit"],
        "SMTP_ADDR": "mailpit:1025",
        "SMTP_USER": "email-worker",
        "SMTP_PASSWORD_FILE": path["smtp"],
        "SMTP_CA_FILE": ca_file,
        "ORDER_PAGE_URL": "http://localhost:8080/orders/",
        "ORDER_TOKEN_KEY_FILE": path["key"],
    }


def same(a: str, b: str) -> bool:
    """Compares two secrets without pytest's diff, which would print both."""
    return a == b


def test_a_valid_environment(environ, files):
    """Every setting is read, and no secret shows in the settings' repr."""
    settings = Settings.from_env(Env(environ))
    assert settings.queue == "email-worker"
    password_matches = same(settings.mailer.password, files["smtp"])
    assert password_matches
    assert all(repr(value) == "'<redacted>'" for value in files.values())
    shown = repr(settings)
    assert not any(value in shown for value in files.values())


@pytest.mark.parametrize(
    "change, want_error",
    [
        ({"SMTP_PASSWORD": "s3cret"}, "SMTP_PASSWORD must not be set"),
        ({"SMTP_ADDR": "mailpit"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": "1025"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": ":1025"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": "mailpit:0"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": "mailpit:70000"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": ":s3cret@mailpit:1025"}, "SMTP_ADDR must be host:port"),
        ({"SMTP_ADDR": "mailpit:1025#x"}, "SMTP_ADDR must be host:port"),
        (
            {"DATABASE_URL": "postgres://email_app@postgres:pg/email"},
            "DATABASE_URL must be",
        ),
        ({"ORDER_PAGE_URL": "http://localhost:http/orders/"}, "with a valid port"),
        ({"LOG_LEVEL": "loud"}, "invalid log level"),
        ({"ORDER_PAGE_URL": "http://shop.example/orders/"}, "must use https"),
        ({"SMTP_CA_FILE": "/absent.crt"}, "SMTP_CA_FILE"),
    ],
)
def test_an_invalid_setting(environ, change, want_error):
    """Each invalid setting stops the worker with its reason."""
    with pytest.raises(ConfigError, match=want_error) as raised:
        Settings.from_env(Env(environ | change))
    assert "s3cret" not in str(raised.value)


def test_every_problem_at_once(environ):
    """Missing settings are reported together."""
    del environ["SMTP_USER"], environ["DATABASE_URL"]
    with pytest.raises(ConfigError) as raised:
        Settings.from_env(Env(environ))
    assert "SMTP_USER is required" in str(raised.value)
    assert "DATABASE_URL is required" in str(raised.value)


@pytest.mark.parametrize(
    "consumer, sweeper, want",
    [(True, True, True), (True, False, False), (False, True, False)],
)
def test_readiness_needs_every_check(consumer, sweeper, want):
    """The worker is ready only while subscribed without an outage and while
    the last sweep reached the database."""
    assert readiness(lambda: consumer, lambda: sweeper)() is want
