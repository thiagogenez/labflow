# Repository Guidelines

## Purpose

LabFlow is the order-processing application that runs on [LabFlow Platform](https://github.com/thiagogenez/labflow-platform). The business layer is thin but realistic: it exists to produce real traffic, data and failure modes for platform and SRE practice. The plan and settled design decisions live in `labflow-platform/docs/`; read them before changing anything.

## Working Agreement

- When unsure about a decision, do not guess: run the `grilling` skill (rounds of numbered questions, each with a recommended answer) and wait for the learner's answers. Record settled decisions in `labflow-platform/docs/decisions.md`.
- Follow `labflow-platform/CONTRIBUTING.md`; the learner is its maintainer.
- The agent writes the application code. The learner reviews design decisions (event contracts, idempotency, retries) and walks through the code, but does not need to write it.
- Code, documentation and commit messages are in English; teaching is in Portuguese.
- Give the learner exactly one command per message, say what it should print, and wait for the output.
- Prefix agent shell commands with `rtk`; use `rtk proxy` when unfiltered output is needed.
- Keep the tree clean: scratch work stays out of the repository.
- `psp-simulator`, `supplier-simulator` and Mailpit stand in for external systems; say so wherever they appear. Never invent results or traffic.

## Service Conventions

- Configuration comes from environment variables, validated at startup; invalid configuration fails fast.
- HTTP services expose `/healthz` (liveness), `/readyz` (dependencies ready) and `/metrics` (Prometheus). Workers expose the same endpoints on an admin port.
- Every outbound call has an explicit timeout. Shut down gracefully on SIGTERM within the termination grace period.
- Logs are structured JSON on stdout with the W3C trace ID as correlation ID. Every service installs the OpenTelemetry SDK and configures its exporter only through the standard `OTEL_*` environment variables. Public entry points start a new trace and keep an inbound `traceparent` only as a link; consumers process each event as a child of its `traceparent`, so one trace ID follows each request, such as a checkout or a refund, through every service it reaches; `subject` ties together the traces of one order. Order, user and event IDs never become metric labels.
- Events are CloudEvents 1.0 in structured JSON mode (message content type `application/cloudevents+json`), described by JSON Schema in `contracts/events/`. `id` is the event ID, `type` carries the major version (`labflow.order.placed.v1`), `subject` is the order ID and `traceparent` carries the trace context. Schemas are closed, and producer tests validate every event against them with format assertion on; consumers never validate, ignore unknown fields and tolerate unknown enum values. Fields are added as optional, never repurposed; an incompatible change is a new major `type`, published alongside the old one until every consumer has moved.
- Events carry only the personal data email-worker needs. Logs identify an event by `id`, `type`, `subject` and trace ID, never by its `data`; among consumers, only email-worker stores `data.customer`; broker queues, dead-letter queues and published outbox rows keep events for at most 7 days.
- Delivery is at least once and unordered. Consumers are idempotent, keeping processed-event records longer than an event can stay in the broker, retry and dead-letter queues combined, and tolerate reordering: they apply only valid state transitions, retry an event whose prerequisite has not arrived yet, or keep aggregates commutative. State changes that emit events use the transactional outbox.
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

Add Python, Go and Dockerfile hooks with the first files of each type.
