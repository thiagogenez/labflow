# Repository Guidelines

## Purpose

LabFlow is the order-processing application that runs on [LabFlow Platform](https://github.com/thiagogenez/labflow-platform). The business layer is thin but realistic: it exists to produce real traffic, data and failure modes for platform and SRE practice. The plan and settled design decisions live in `labflow-platform/docs/`; read them before changing anything.

## Working Agreement

- When unsure about a decision, do not guess: run the `grilling` skill (rounds of numbered questions, each with a recommended answer) and wait for the learner's answers. Record settled decisions in `labflow-platform/docs/decisions.md`.
- The agent writes the application code. The learner reviews design decisions (event contracts, idempotency, retries) and walks through the code, but does not need to write it.
- Code, documentation and commit messages are in English; teaching is in Portuguese.
- Give the learner exactly one command per message, say what it should print, and wait for the output.
- Prefix agent shell commands with `rtk`; use `rtk proxy` when unfiltered output is needed.
- Keep the tree clean: scratch work stays out of the repository.
- `psp-simulator` and Mailpit stand in for external systems; say so wherever they appear. Never invent results or traffic.

## Service Conventions

- Configuration comes from environment variables, validated at startup; invalid configuration fails fast.
- HTTP services expose `/healthz` (liveness), `/readyz` (dependencies ready) and `/metrics` (Prometheus). Workers expose the same endpoints on an admin port.
- Every outbound call has an explicit timeout. Shut down gracefully on SIGTERM within the termination grace period.
- Logs are structured JSON on stdout with a correlation ID. Order, user and event IDs never become metric labels.
- Events are JSON envelopes described by JSON Schema in `contracts/`. Every event has an `event_id`. Fields are added, never repurposed; consumers ignore unknown fields.
- Delivery is at least once. Consumers are idempotent (processed-event records); state changes that emit events use the transactional outbox.
- The broker is selected by `BROKER` (`rabbitmq`, `kafka` or `sqs`) behind one small adapter per language. Exactly one broker per deployment.
- Card numbers never reach LabFlow services; only provider tokens do.

## Python Rules

- Python 3.14 in a repository-local `.venv` managed by uv. Dependencies are hash-pinned (`--generate-hashes`) and installed from binary wheels only.
- Ruff with its upstream default rules is the linter; Black with its default stable style (88 columns) is the only formatter. Do not also run Ruff formatting or add rule overrides without an agreed reason.
- Pylint default diagnostics run on every versioned Python file, including scripts. The editor, the hook and CI use the same configuration.
- Install each Python service package in editable mode. Never inject `sys.path` or suppress E0401 to hide an import problem.
- Tests are `tests/test_*.py` with pytest: plain `assert`, `pytest.raises` for failures, `parametrize` for input cases, fixtures for shared setup. Preserve existing cases when refactoring.
- `snake_case` modules, functions and script filenames; `PascalCase` classes.

## Go Rules

- Go 1.27.x, pinned with `go` and `toolchain` in each service's `go.mod`; one module per service.
- gofmt and goimports formatting; golangci-lint v2 with the `standard` linter set; `go test -race ./...`.
- The editor matches CI: VS Code uses golangci-lint v2 as its lint tool, so it underlines exactly what CI rejects. Shared settings live in `.vscode/`.
- Table-driven tests with the standard `testing` package. Wrap errors with `%w`; pass `context.Context` first on I/O paths; no mutable globals.

## Images

Multi-arch (arm64, amd64), built in CI on native runners, scanned with Trivy (failing on critical findings), signed with cosign keyless, and promoted by digest through a pull request to `labflow-platform`. Go services use distroless or static base images; Python services use slim images. Containers run as non-root with a read-only root filesystem.

## Quality Gates

- Run `make setup` once per clone; run `make check` before asking for a commit. CI runs the same hook configuration on native ARM64.
- Current hooks: file hygiene, gitleaks, actionlint and commit-message checks. Python, Go, Dockerfile and shell hooks are added with the first files of each type.
- Never bypass hooks (`--no-verify`, `SKIP`, suppressions); fix the finding.

## Commits and Pull Requests

These rules are identical in `labflow` and `labflow-platform`; change them in both.

- Before every commit, documentation included, three fresh agents review the staged diff independently. They get the diff, both repositories' `AGENTS.md`, `labflow-platform/docs/plan.md` and `docs/decisions.md`, and may open any file the change refers to, but never the author's reasoning. Reviewer A checks correctness, consistency, duplication and reinvented wheels (official documentation, Context7); B checks over-engineering and bloat (`ponytail-review`); C checks security and operability (secrets, least privilege, failure handling, timeouts, resource limits, observability, rollback). Each does a second round that re-verifies its own findings and looks for misses. Findings raised by two or more reviewers are consensus. Present every finding to the learner with an apply/reject verdict and reason; a fix that changes logic gets one re-review, then goes to the learner.
- The learner approves each commit message and pull request description. The agent then commits, pushes to a topic branch (never to `main`) and opens the pull request; the learner merges it with a merge commit titled by the pull request (no squash, no rebase). Prefer few, step-sized commits.
- Conventional Commits, subject at most 72 characters. Write each body paragraph as one unwrapped line; GitHub wraps it. Never add `Co-authored-by`, "Generated with" or any other attribution line to commits or pull requests. The commit-msg hooks check the Conventional Commits header, the subject length and `Co-authored-by` trailers, and CI runs them on every pull-request commit; the remaining rules are checked in review.
- Pull requests state what changed, how it was verified, what remains limited, and a Review section summarizing the findings and their resolution.
