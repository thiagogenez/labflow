"""Order links and their configuration (D56)."""

import base64
import json
from pathlib import Path

import pytest
from labflow.config import ConfigError, Env

from email_worker.links import Links, token

VECTOR = Path(__file__).resolve().parents[3] / "contracts/order-access-token.json"


def test_token_matches_the_shared_test_vector():
    """email-worker derives the token order-api issues."""
    vector = json.loads(VECTOR.read_text(encoding="utf-8"))
    key = base64.b64decode(vector["key_base64"])
    assert token(key, vector["order_id"]) == vector["expected"]
    assert token(key, vector["order_id"].upper()) == vector["expected"]


@pytest.fixture(name="key_file")
def fixture_key_file(tmp_path, token_key):
    """A file holding a valid key, as base64."""
    path = tmp_path / "order-token-key"
    path.write_text(base64.b64encode(token_key).decode() + "\n", encoding="utf-8")
    return str(path)


@pytest.mark.parametrize(
    "url",
    ["https://shop.example/orders/", "http://localhost:8080/orders/"],
)
def test_order_page_url_accepted(key_file, url):
    """HTTPS anywhere, or HTTP on localhost."""
    env = Env({"ORDER_PAGE_URL": url, "ORDER_TOKEN_KEY_FILE": key_file})
    Links.from_env(env)
    env.raise_errors()


@pytest.mark.parametrize(
    "url, want_error",
    [
        ("", "ORDER_PAGE_URL is required"),
        ("/orders/", "must be an absolute http or https URL"),
        ("ftp://shop.example/orders/", "must be an absolute http or https URL"),
        ("https://shop.example:shop/orders/", "with a valid port"),
        ("https://shop.example:70000/orders/", "with a valid port"),
        ("https://user:s3cret@shop.example/orders/", "must not carry credentials"),
        ("https://shop.example/orders", "must end in /"),
        ("https://shop.example/orders/?a=1", "carry no query or fragment"),
        ("https://shop.example/orders/#x", "carry no query or fragment"),
        ("http://shop.example/orders/", "must use https on any host but localhost"),
        ("http://127.0.0.1:8080/orders/", "must use https on any host but localhost"),
    ],
)
def test_order_page_url_refused(key_file, url, want_error):
    """Anything a link cannot safely be built on is refused at startup."""
    env = Env({"ORDER_PAGE_URL": url, "ORDER_TOKEN_KEY_FILE": key_file})
    Links.from_env(env)
    with pytest.raises(ConfigError, match=want_error) as raised:
        env.raise_errors()
    assert "s3cret" not in str(raised.value)


@pytest.mark.parametrize(
    "content, want_error",
    [
        ("not base64!", "ORDER_TOKEN_KEY_FILE must hold base64"),
        (base64.b64encode(b"short").decode(), "at least 32 bytes"),
    ],
)
def test_token_key_refused(tmp_path, content, want_error):
    """The key must be base64 of at least 32 bytes, as order-api requires."""
    path = tmp_path / "key"
    path.write_text(content, encoding="utf-8")
    env = Env(
        {
            "ORDER_PAGE_URL": "https://shop.example/orders/",
            "ORDER_TOKEN_KEY_FILE": str(path),
        }
    )
    Links.from_env(env)
    with pytest.raises(ConfigError, match=want_error):
        env.raise_errors()
