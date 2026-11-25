"""Service configuration from environment variables, validated at startup.

It follows libs/go/config's rules for the getters it has: every getter
records problems instead of raising on the first one, so a service reports
every invalid setting at startup and exits once, and a secret comes only from
the file a NAME_FILE variable names. A getter is added, with Go's test cases,
when a Python service first needs it.
"""

import os
from collections.abc import Mapping
from urllib.parse import SplitResult, urlsplit


class ConfigError(Exception):
    """One or more settings are missing or invalid."""


class Env:
    """Reads settings from a mapping of variables, the process environment by
    default, collecting the problems found."""

    def __init__(self, environ: Mapping[str, str] | None = None):
        self._environ = os.environ if environ is None else environ
        self._errors: list[str] = []

    def _value(self, name: str) -> str:
        return self._environ.get(name, "").strip()

    def string(self, name: str, default: str) -> str:
        """Returns the variable, or default when it is unset or empty."""
        return self._value(name) or default

    def required(self, name: str) -> str:
        """Returns the variable and records an error when it is unset or empty."""
        value = self._value(name)
        if not value:
            self._errors.append(f"{name} is required")
        return value

    def secret(self, name: str) -> str:
        """Returns the contents of the file that NAME_FILE names, less one
        trailing newline, and records an error when NAME_FILE is unset, the
        file is unreadable or empty, or NAME itself is set, even to nothing:
        a secret in a variable's value is refused, not used. No error repeats
        the secret."""
        if name in self._environ:
            self._errors.append(
                f"{name} must not be set; put the secret in a file and set {name}_FILE"
            )
            return ""
        path = self.required(f"{name}_FILE")
        if not path:
            return ""
        try:
            # newline="" keeps a carriage return, which belongs to the secret.
            with open(path, encoding="utf-8", newline="") as f:
                data = f.read()
        except (OSError, UnicodeDecodeError) as e:
            self._errors.append(f"{name}_FILE: {e}")
            return ""
        # Only the newline an editor may add is dropped, as psql and the Go
        # services do: every other byte belongs to the secret.
        value = data.removesuffix("\n")
        if not value:
            self._errors.append(f"{name}_FILE {path} is empty")
        return value

    def url(self, name: str) -> SplitResult | None:
        """Parses a required absolute http or https URL without credentials."""
        value = self.required(name)
        if not value:
            return None
        u = split_url(value, ("http", "https"))
        if u is None:
            self._errors.append(
                f"{name} must be an absolute http or https URL with a valid port"
            )
            return None
        if "@" in u.netloc:
            # A credential belongs in a secret file, and an error must not
            # repeat it.
            self._errors.append(f"{name} must not carry credentials")
            return None
        return u

    def check(self, ok: bool, message: str) -> None:
        """Records message when a cross-field rule fails."""
        if not ok:
            self._errors.append(message)

    def raise_errors(self) -> None:
        """Raises ConfigError naming every problem found so far, if any."""
        if self._errors:
            raise ConfigError("\n".join(self._errors))


def split_url(raw: str, schemes: tuple[str, ...]) -> SplitResult | None:
    """Parses an absolute URL with one of the schemes, a host and, if any, a
    port from 1 to 65535; None for anything else."""
    try:
        u = urlsplit(raw)
        # .port raises ValueError for a port that is no port, as Go's
        # url.Parse refuses one.
        valid = u.scheme in schemes and bool(u.hostname) and u.port != 0
    except ValueError:
        return None
    return u if valid else None
