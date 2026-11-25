"""The admin port of a worker: /healthz, /readyz and /metrics, as the Go
services serve them."""

import threading
import urllib.request
from collections.abc import Callable
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

_PROBE_TIMEOUT_SECONDS = 2


def _split(addr: str) -> tuple[str, int]:
    """Splits ":8080" or "host:8080"; an empty host listens everywhere."""
    host, _, port = addr.rpartition(":")
    return host, int(port)


class AdminServer:
    """Serves the probes and metrics in a background thread. ready decides
    /readyz; /healthz answers while the process runs."""

    def __init__(self, addr: str, ready: Callable[[], bool]):
        class Handler(BaseHTTPRequestHandler):
            """Answers the three admin routes."""

            timeout = 5  # an idle connection does not hold a thread forever

            def do_GET(self) -> None:  # pylint: disable=invalid-name
                """Serves GET requests; the name is http.server's."""
                if self.path == "/healthz":
                    self._reply(HTTPStatus.NO_CONTENT)
                elif self.path == "/readyz":
                    self._reply(
                        HTTPStatus.NO_CONTENT
                        if ready()
                        else HTTPStatus.SERVICE_UNAVAILABLE
                    )
                elif self.path == "/metrics":
                    self._reply(HTTPStatus.OK, generate_latest(), CONTENT_TYPE_LATEST)
                else:
                    self._reply(HTTPStatus.NOT_FOUND)

            def _reply(
                self, status: HTTPStatus, body: bytes = b"", content_type: str = ""
            ) -> None:
                self.send_response(status)
                if content_type:
                    self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(
                self, format: str, *args: object
            ) -> None:  # pylint: disable=redefined-builtin
                """Probes every few seconds would flood the log."""

        self._server = ThreadingHTTPServer(_split(addr), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    @property
    def port(self) -> int:
        """The bound port, useful when listening on port 0."""
        return self._server.server_address[1]

    def start(self) -> None:
        """Starts serving."""
        self._thread.start()

    def stop(self) -> None:
        """Stops serving and closes the listener."""
        self._server.shutdown()
        self._server.server_close()


def probe(addr: str) -> None:
    """The container health check: raises unless /readyz on the local port
    answers 204."""
    _, port = _split(addr)
    with urllib.request.urlopen(
        f"http://127.0.0.1:{port}/readyz", timeout=_PROBE_TIMEOUT_SECONDS
    ) as response:
        if response.status != HTTPStatus.NO_CONTENT:
            raise RuntimeError(f"readyz returned {response.status}")
