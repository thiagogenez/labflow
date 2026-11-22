.PHONY: help setup check test

help:
	@printf '%s\n' \
	  'make setup  Create the pinned tool environment and install the Git hooks' \
	  'make check  Run every hook against all files, as CI does' \
	  'make test   Run the tests of every Go module with the race detector (needs Docker)'

setup:
	uv venv --allow-existing --python "$$(cat .python-version)" .venv
	uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: -r requirements-dev.txt
	.venv/bin/pre-commit install --install-hooks

check:
	.venv/bin/pre-commit run --all-files --show-diff-on-failure

test:
	git ls-files --cached --others --exclude-standard -- '*/go.mod' | xargs -n1 dirname | while read -r dir; do \
	  echo "== $$dir"; (cd "$$dir" && go test -race -count=1 ./...) || exit 1; \
	done
