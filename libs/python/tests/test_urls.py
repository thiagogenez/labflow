"""Connection settings: the password only from its file, never from the URL,
and no option that could carry another credential."""

import pytest
from psycopg.conninfo import conninfo_to_dict

from labflow.config import ConfigError, Env
from labflow.postgres import dsn_from_env
from labflow.rabbitmq import parameters, url_from_env

ONLY_OPTIONS = "may set only the query options"


@pytest.mark.parametrize(
    "url, want_error",
    [
        ("postgres://orders_app:0ldPass@postgres/orders", "must not carry a password"),
        ("postgres://orders_app@postgres/orders#0ldPass", "must not carry a password"),
        ("postgres://orders_app@postgres/orders?password=0ldPass", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?%70assword=0ldPass", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?%20password%20=0ldPass", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?password=", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?passfile=/tmp/pg", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?sslpassword=0ldPass", ONLY_OPTIONS),
        (
            "postgres://orders_app@postgres/orders?oauth_client_secret=0ldPass",
            ONLY_OPTIONS,
        ),
        ("postgres://orders_app@postgres/orders?sslmode", ONLY_OPTIONS),
        ("postgres://orders_app@postgres/orders?%20sslmode=disable", ONLY_OPTIONS),
        (
            "postgres://orders_app@postgres/orders?application_name=%ff",
            "is not a connection",
        ),
        ("postgres://postgres/orders", "must be a postgres:// URL"),
        ("postgres://orders_app@postgres:pg/orders", "must be a postgres:// URL"),
        ("postgres://orders_app@postgres:0/orders", "must be a postgres:// URL"),
        ("mysql://orders_app@db/orders", "must be a postgres:// URL"),
    ],
)
def test_dsn_refuses(secret_file, url, want_error):
    """A URL carrying a password or any option but a few non-secret ones, or
    no valid PostgreSQL URL, is refused, and the error repeats neither the
    URL's password nor the file's."""
    env = Env(
        {"DATABASE_URL": url, "DATABASE_PASSWORD_FILE": secret_file("p@ss/w0rd\n")}
    )
    assert dsn_from_env(env, "DATABASE_URL") == ""
    with pytest.raises(ConfigError, match="DATABASE_URL " + want_error) as raised:
        env.raise_errors()
    assert "0ldPass" not in str(raised.value) and "p@ss" not in str(raised.value)


def test_dsn_takes_the_password_from_its_file(secret_file):
    """The connection gets the file's password, whatever it contains, and the
    URL's non-secret options."""
    env = Env(
        {
            "DATABASE_URL": "postgres://orders_app@postgres:5432/orders?sslmode=disable",
            "DATABASE_PASSWORD_FILE": secret_file("p@ss/w0rd\n"),
        }
    )
    dsn = dsn_from_env(env, "DATABASE_URL")
    env.raise_errors()
    assert conninfo_to_dict(dsn) == {
        "user": "orders_app",
        "password": "p@ss/w0rd",
        "host": "postgres",
        "port": "5432",
        "dbname": "orders",
        "sslmode": "disable",
    }


def test_broker_url_takes_the_password_from_its_file(secret_file):
    """The broker URL gets the file's password, escaped."""
    env = Env(
        {
            "BROKER": "rabbitmq",
            "RABBITMQ_URL": "amqp://order-api@rabbitmq:5672/labflow",
            "RABBITMQ_PASSWORD_FILE": secret_file("p@ss/w0rd\n"),
        }
    )
    assert url_from_env(env) == "amqp://order-api:p%40ss%2Fw0rd@rabbitmq:5672/labflow"
    env.raise_errors()


NOTHING_ELSE = "must carry no password, query or fragment"


@pytest.mark.parametrize(
    "broker, url, want_error",
    [
        ("", "amqp://order-api@rabbitmq/labflow", "BROKER is required"),
        (
            "kafka",
            "amqp://order-api@rabbitmq/labflow",
            "BROKER must be rabbitmq, got 'kafka'",
        ),
        ("rabbitmq", "", "RABBITMQ_URL is required"),
        ("rabbitmq", "amqp://order-api:0ldPass@rabbitmq/labflow", NOTHING_ELSE),
        (
            "rabbitmq",
            "amqp://order-api@rabbitmq/labflow?password=0ldPass",
            NOTHING_ELSE,
        ),
        ("rabbitmq", "amqp://order-api@rabbitmq/labflow?ssl_options=x", NOTHING_ELSE),
        ("rabbitmq", "amqp://order-api@rabbitmq/labflow#0ldPass", NOTHING_ELSE),
        ("rabbitmq", "amqp://rabbitmq/labflow", "RABBITMQ_URL must be"),
        ("rabbitmq", "http://order-api@rabbitmq", "RABBITMQ_URL must be"),
        ("rabbitmq", "amqp://order-api@rabbitmq:70000/labflow", "RABBITMQ_URL must be"),
        ("rabbitmq", "amqp://order-api@rabbitmq:0/labflow", "RABBITMQ_URL must be"),
        ("rabbitmq", "amqp:rabbitmq", "RABBITMQ_URL must be"),
        ("rabbitmq", "amqp://order-api@rabbitmq/lab%zzflow", "RABBITMQ_URL must be"),
        ("rabbitmq", "amqp://admin@rabbitmq/labflow%", "RABBITMQ_URL must be"),
    ],
)
def test_broker_url_refuses(secret_file, broker, url, want_error):
    """Another broker, a URL carrying a password or any option, or no valid
    AMQP URL is an error that repeats no password."""
    env = Env(
        {
            "BROKER": broker,
            "RABBITMQ_URL": url,
            "RABBITMQ_PASSWORD_FILE": secret_file("p@ss/w0rd\n"),
        }
    )
    url_from_env(env)
    with pytest.raises(ConfigError, match=want_error) as raised:
        env.raise_errors()
    assert "0ldPass" not in str(raised.value) and "p@ss" not in str(raised.value)


@pytest.mark.parametrize(
    "path, vhost",
    [
        ("/labflow", "labflow"),
        ("/team/orders", "team/orders"),
        ("/%2F", "/"),
        ("", "/"),
    ],
)
def test_parameters_read_the_whole_path_as_the_virtual_host(path, vhost):
    """The virtual host is the whole path, unescaped, as the Go services read
    it; pika alone would stop at the first slash."""
    p = parameters(f"amqp://email-worker:p%40ss@rabbitmq:5672{path}")
    assert p.virtual_host == vhost
    assert (p.host, p.port) == ("rabbitmq", 5672)
    assert (p.credentials.username, p.credentials.password) == ("email-worker", "p@ss")
    assert p.ssl_options is None


def test_parameters_use_tls_for_amqps():
    """amqps connects over TLS on 5671, verifying the broker's name."""
    p = parameters("amqps://email-worker:x@rabbitmq.example/labflow")
    assert p.port == 5671
    assert (
        p.ssl_options is not None
        and p.ssl_options.server_hostname == "rabbitmq.example"
    )
