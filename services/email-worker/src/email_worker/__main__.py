"""email-worker: sends the customer emails LabFlow's events call for, through
Mailpit, the lab's simulated mail server (D69).

Usage: service [serve | healthcheck], or python -m email_worker
"""

import logging
import signal
import sys
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from importlib.metadata import version

from labflow import admin, logs, postgres, rabbitmq, telemetry
from labflow.config import ConfigError, Env

from email_worker.handler import Handler, Sweeper
from email_worker.links import Links
from email_worker.mail import Mailer, smtp_address
from email_worker.store import Store

SERVICE = "email-worker"


@dataclass(frozen=True)
class Settings:
    """Everything email-worker reads from its environment, validated."""

    log_level: int
    admin_addr: str
    dsn: str = field(repr=False)
    broker_url: str = field(repr=False)
    queue: str
    mailer: Mailer
    links: Links = field(repr=False)

    @classmethod
    def from_env(cls, env: Env) -> Settings:
        """Reads every setting, raising ConfigError naming each problem."""
        level = logging.INFO
        try:
            level = logs.parse_level(env.string("LOG_LEVEL", "info"))
        except ValueError as e:
            env.check(False, str(e))
        admin_addr = env.string("HTTP_ADDR", ":8080")
        dsn = postgres.dsn_from_env(env, "DATABASE_URL")
        broker_url = rabbitmq.url_from_env(env)
        queue = env.string("RABBITMQ_QUEUE", "email-worker")
        smtp_addr = env.required("SMTP_ADDR")
        env.check(
            not smtp_addr or smtp_address(smtp_addr) is not None,
            "SMTP_ADDR must be host:port, with a port from 1 to 65535",
        )
        smtp = (smtp_addr, env.required("SMTP_USER"), env.secret("SMTP_PASSWORD"))
        ca_file = env.required("SMTP_CA_FILE")
        links = Links.from_env(env)
        env.raise_errors()
        try:
            mailer = Mailer(*smtp, ca_file)
        except OSError as e:  # an unreadable or invalid certificate file
            raise ConfigError(f"SMTP_CA_FILE: {e}") from None
        return cls(level, admin_addr, dsn, broker_url, queue, mailer, links)


def readiness(*checks: Callable[[], bool]) -> Callable[[], bool]:
    """Ready while every check holds: subscribed and holding no message
    through an outage, and the last sweep reached the database."""
    return lambda: all(check() for check in checks)


def serve() -> None:
    """Consumes the email-worker queue until SIGTERM or SIGINT."""
    settings = Settings.from_env(Env())
    log = logs.setup(SERVICE, settings.log_level)
    flush_traces = telemetry.setup(SERVICE, version(SERVICE))
    # The sweep runs beside the handler, so each has its own connection.
    store, sweep_store = Store(settings.dsn), Store(settings.dsn)
    handler = Handler(store, settings.mailer, settings.links, log)
    consumer = rabbitmq.Consumer(settings.broker_url, settings.queue, handler, log)
    sweeper = Sweeper(sweep_store, log)
    server = admin.AdminServer(
        settings.admin_addr, readiness(consumer.ready, sweeper.healthy)
    )

    stop = threading.Event()
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: stop.set())
    server.start()
    sweeping = threading.Thread(target=sweeper.run, args=(stop,), daemon=True)
    sweeping.start()
    log.info("consuming", extra={"queue": settings.queue})
    try:
        consumer.run(stop)
    finally:
        stop.set()
        server.stop()
        # Within the grace period: a batch still running is abandoned, and
        # deleted again by the next sweep.
        sweeping.join(5)
        store.close()
        sweep_store.close()
        flush_traces()
        log.info("stopped")


def main(args: list[str]) -> int:
    """Runs the command in args, returning the exit status."""
    command = args[0] if args else "serve"
    try:
        if command == "serve":
            serve()
        elif command == "healthcheck":
            admin.probe(Env().string("HTTP_ADDR", ":8080"))
        else:
            print(
                f"{SERVICE}: unknown command {command!r}: use serve or healthcheck",
                file=sys.stderr,
            )
            return 2
    except ConfigError as e:
        print(f"{SERVICE}: invalid configuration:\n{e}", file=sys.stderr)
        return 1
    except OSError as e:  # the health check could not reach the admin port
        print(f"{SERVICE}: {e}", file=sys.stderr)
        return 1
    return 0


def run() -> None:
    """The `service` command."""
    sys.exit(main(sys.argv[1:]))


if __name__ == "__main__":
    run()
