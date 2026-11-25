.PHONY: help setup venv lock up rotate-secrets check test test-go test-python test-leftovers

# Python packages: the shared library, then every service with a pyproject.toml.
python_services := $(patsubst %/pyproject.toml,%,$(wildcard services/*/pyproject.toml))
python_packages := libs/python $(python_services)
python_requirements := $(addsuffix /requirements.txt,$(python_services))
python_version := $(shell cat .python-version)
compile := uv pip compile --quiet --python-version $(python_version) --universal --generate-hashes --only-binary :all:

help:
	@printf '%s\n' \
	  'make setup           Create the pinned tool environment, install the Python packages and the Git hooks' \
	  'make lock            Recompile every hash-pinned requirements file after a dependency change' \
	  'make up              Generate any missing local secret, then build and start the stack' \
	  'make rotate-secrets  Replace every local secret and restart the stack with them' \
	  'make check           Run the pre-commit-stage hooks against all files' \
	  'make test            Run every Go module with the race detector, then every Python package (needs Docker)'

setup: venv
	.venv/bin/pre-commit install --install-hooks

# Every third-party package is a hash-checked wheel; LabFlow's own packages are
# installed editable, built offline by the hash-checked build backend.
venv:
	uv venv --allow-existing --python $(python_version) .venv
	uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: \
	  -r requirements-dev.txt -r requirements-build.txt $(addprefix -r ,$(python_requirements))
	uv pip install --python .venv/bin/python --no-deps --no-build-isolation --no-index $(addprefix -e ,$(python_packages))

# A service's pins come from its pyproject.toml and libs/python's, which it
# installs from source; the development tools are constrained to the other
# pins, so one environment holds them all.
lock:
	for dir in $(python_services); do \
	  (cd "$$dir" && $(compile) --no-emit-package labflow pyproject.toml -o requirements.txt) || exit 1; \
	done
	$(compile) requirements-build.in -o requirements-build.txt
	$(compile) $(addprefix -c ,$(python_requirements)) -c requirements-build.txt requirements-dev.in -o requirements-dev.txt

# The generator writes a new .secrets/generation whenever the secrets change;
# Compose labels the services with it and recreates those whose label differs.
# A new .secrets/ is kept out of macOS backups, which would outlive rotation.
secrets = test -d .secrets || { mkdir -m 700 .secrets && { ! command -v tmutil >/dev/null || tmutil addexclusion .secrets; }; }; \
	go -C libs/go run ./cmd/local-secrets -root ../..
start = generation=$$(cat .secrets/generation) && LABFLOW_SECRETS_GENERATION=$$generation docker compose up --wait

up:
	$(secrets)
	$(start) --build

# Builds first, so a failed build stops before any secret changes.
rotate-secrets:
	docker compose build
	$(secrets) -rotate
	$(start)

check:
	.venv/bin/pre-commit run --all-files --show-diff-on-failure

# Test containers carry the label labflow.test with their test process's ID; a
# run killed before its own cleanup leaves them behind, and the next run
# removes those whose process is gone, with their anonymous volumes.
test: test-go test-python

test-leftovers:
	docker ps -a --filter label=labflow.test --format '{{.ID}} {{.Label "labflow.test"}}' | \
	  while read -r id pid; do kill -0 "$$pid" 2>/dev/null || docker rm --force --volumes "$$id" >/dev/null; done

test-go: test-leftovers
	git ls-files --cached --others --exclude-standard -- '*/go.mod' | xargs -n1 dirname | while read -r dir; do \
	  echo "== $$dir"; (cd "$$dir" && go test -race -count=1 ./...) || exit 1; \
	done

test-python: test-leftovers
	for dir in $(python_packages); do \
	  echo "== $$dir"; (cd "$$dir" && "$(CURDIR)/.venv/bin/pytest" -q) || exit 1; \
	done
