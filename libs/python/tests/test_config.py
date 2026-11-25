"""The same cases as libs/go/config's tests."""

import pytest

from labflow.config import ConfigError, Env


@pytest.mark.parametrize(
    "environ, read, want, want_error",
    [
        ({}, lambda e: e.string("A", "x"), "x", None),
        ({"A": " y "}, lambda e: e.string("A", "x"), "y", None),
        ({}, lambda e: e.required("A"), "", "A is required"),
        ({"A": "  "}, lambda e: e.required("A"), "", "A is required"),
        (
            {"U": "http://psp:8080"},
            lambda e: e.url("U").geturl(),
            "http://psp:8080",
            None,
        ),
        ({"U": "psp:8080"}, lambda e: e.url("U"), None, "U must be an absolute http"),
        ({"U": "http://psp:http"}, lambda e: e.url("U"), None, "with a valid port"),
        ({"U": "http://psp:70000"}, lambda e: e.url("U"), None, "with a valid port"),
        (
            {"U": "http://user:s3cret@psp:8080"},
            lambda e: e.url("U"),
            None,
            "U must not carry credentials",
        ),
        ({}, lambda e: e.check(False, "keys must differ"), None, "keys must differ"),
    ],
)
def test_env(environ, read, want, want_error):
    """Each getter returns the value or its default, recording what is wrong."""
    env = Env(environ)
    assert read(env) == want
    if want_error is None:
        env.raise_errors()
    else:
        with pytest.raises(ConfigError, match=want_error) as raised:
            env.raise_errors()
        assert "s3cret" not in str(raised.value)


def test_env_reports_every_problem():
    """One error names every problem, so a service exits once."""
    env = Env({"U": "psp"})
    env.required("A")
    env.required("B")
    env.url("U")
    with pytest.raises(ConfigError) as raised:
        env.raise_errors()
    for want in ("A is required", "B is required", "U must be an absolute"):
        assert want in str(raised.value)


@pytest.mark.parametrize(
    "value, content, want, want_error",
    [
        (None, " s3cret value \n", " s3cret value ", None),
        (None, "s3cret\r\n", "s3cret\r", None),
        ("s3cret value", "s3cret\n", "", "S must not be set; put the secret in a file"),
        ("", "s3cret\n", "", "S must not be set"),
        (None, "\n", "", "is empty"),
    ],
)
def test_secret(secret_file, value, content, want, want_error):
    """A secret comes from its file, less one newline, never from NAME."""
    environ = {"S_FILE": secret_file(content)}
    if value is not None:
        environ["S"] = value
    env = Env(environ)
    assert env.secret("S") == want
    if want_error is None:
        env.raise_errors()
    else:
        with pytest.raises(ConfigError, match=want_error) as raised:
            env.raise_errors()
        assert "s3cret" not in str(raised.value)


@pytest.mark.parametrize(
    "environ, want_error",
    [({}, "S_FILE is required"), ({"S_FILE": "/absent/secret"}, "S_FILE: ")],
)
def test_secret_needs_its_file(environ, want_error):
    """A secret whose file is not named or cannot be read is an error."""
    env = Env(environ)
    assert env.secret("S") == ""
    with pytest.raises(ConfigError, match=want_error):
        env.raise_errors()
