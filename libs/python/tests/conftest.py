"""Fixtures shared by libs/python's tests."""

import pytest


@pytest.fixture(name="secret_file")
def fixture_secret_file(tmp_path):
    """Writes a secret file and returns its path."""

    def write(content: str, name: str = "secret") -> str:
        path = tmp_path / name
        path.write_bytes(content.encode())
        return str(path)

    return write
