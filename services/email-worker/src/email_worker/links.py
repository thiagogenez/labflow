"""Links to the order page, carrying the order access token (D56)."""

import base64
import binascii
import hashlib
import hmac
import uuid

from labflow.config import Env

MIN_KEY_BYTES = 32


def token(key: bytes, order_id: str) -> str:
    """The order access token: HMAC-SHA-256 over the lowercase, hyphenated
    order UUID, as unpadded base64url, the same token order-api issues."""
    mac = hmac.new(key, str(uuid.UUID(order_id)).encode(), hashlib.sha256).digest()
    return base64.urlsafe_b64encode(mac).rstrip(b"=").decode()


class Links:
    """Builds order links from ORDER_PAGE_URL and the token key."""

    def __init__(self, page_url: str, key: bytes):
        self._page_url = page_url
        self._key = key

    def order(self, order_id: str) -> str:
        """The order page link, with the token in the fragment, which browsers
        never send to a server."""
        return (
            f"{self._page_url}{uuid.UUID(order_id)}#token={token(self._key, order_id)}"
        )

    @classmethod
    def from_env(cls, env: Env) -> Links:
        """Reads ORDER_PAGE_URL and the key ORDER_TOKEN_KEY_FILE holds."""
        page = env.url("ORDER_PAGE_URL")
        if page is not None:
            env.check(
                page.path.endswith("/") and not page.query and not page.fragment,
                "ORDER_PAGE_URL must end in / and carry no query or fragment",
            )
            env.check(
                page.scheme == "https" or page.hostname == "localhost",
                "ORDER_PAGE_URL must use https on any host but localhost",
            )
        raw_key = env.secret("ORDER_TOKEN_KEY")
        key = b""
        if raw_key:
            try:
                key = base64.b64decode(raw_key, validate=True)
            except binascii.Error:
                env.check(False, "ORDER_TOKEN_KEY_FILE must hold base64")
            else:
                env.check(
                    len(key) >= MIN_KEY_BYTES,
                    f"the order token key must be at least {MIN_KEY_BYTES} bytes",
                )
        return cls(page.geturl() if page else "", key)
