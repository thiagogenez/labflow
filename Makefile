.PHONY: help setup check check-staged test

help:
	@printf '%s\n' \
	  'make setup         Install pinned tools and activate the local commit hook' \
	  'make check         Check indexed code working copies and run tests' \
	  'make check-staged  Check staged contents as the commit hook does' \
	  'make test          Run the pytest suite'

setup:
	uv venv --allow-existing --python "$$(cat .python-version)" .venv
	uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: -r requirements-dev.txt
	uv pip install --python .venv/bin/python --no-build-isolation --no-deps -e .
	bash .github/scripts/install-shellcheck.sh
	.venv/bin/python -m pre_commit install

check:
	.venv/bin/python -m pre_commit run --all-files --show-diff-on-failure

check-staged:
	.venv/bin/python -m pre_commit run --show-diff-on-failure

test:
	.venv/bin/python -m pytest -v
