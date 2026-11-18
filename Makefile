.PHONY: help setup check

help:
	@printf '%s\n' \
	  'make setup  Create the pinned tool environment and install the Git hooks' \
	  'make check  Run every hook against all files, as CI does'

setup:
	uv venv --allow-existing --python "$$(cat .python-version)" .venv
	uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: -r requirements-dev.txt
	.venv/bin/pre-commit install --install-hooks

check:
	.venv/bin/pre-commit run --all-files --show-diff-on-failure
