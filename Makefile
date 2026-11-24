.PHONY: help setup up rotate-secrets check test

help:
	@printf '%s\n' \
	  'make setup           Create the pinned tool environment and install the Git hooks' \
	  'make up              Generate any missing local secret, then build and start the stack' \
	  'make rotate-secrets  Replace every local secret and restart the stack with them' \
	  'make check           Run the pre-commit-stage hooks against all files' \
	  'make test            Run the tests of every Go module with the race detector (needs Docker)'

setup:
	uv venv --allow-existing --python "$$(cat .python-version)" .venv
	uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: -r requirements-dev.txt
	.venv/bin/pre-commit install --install-hooks

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
test:
	docker ps -a --filter label=labflow.test --format '{{.ID}} {{.Label "labflow.test"}}' | \
	  while read -r id pid; do kill -0 "$$pid" 2>/dev/null || docker rm --force --volumes "$$id" >/dev/null; done
	git ls-files --cached --others --exclude-standard -- '*/go.mod' | xargs -n1 dirname | while read -r dir; do \
	  echo "== $$dir"; (cd "$$dir" && go test -race -count=1 ./...) || exit 1; \
	done
