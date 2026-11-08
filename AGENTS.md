# Repository Guidelines

## Scope & Learning Workflow

LabFlow is a public Platform Engineering/SRE learning lab. Read [the plan](docs/plan.md) and [preflight](docs/preflight.md) before changing anything. Work only in this repository and one agreed milestone at a time. Explain the proposal, implement that step, verify it, compare expected and observed behavior, discuss limitations, ask a short comprehension question, and wait before advancing. Keep code and documentation in English; teach in Portuguese. Give the learner one command at a time and await its output. Prefix agent shell commands with `rtk`; use `rtk proxy` when unfiltered output is necessary.

## Product Engineering Standard

Treat LabFlow as a maintained job-processing product. Use the engineering discipline expected of a production service while keeping the public learning purpose explicit. Use product language in code, APIs and operational documentation; avoid throwaway implementations and descriptions such as "just a dummy". Clearly distinguish implemented, locally tested, simulated and planned capabilities. Never invent production deployments, customers, traffic, availability, scale or benchmark results.

- Build small, cohesive modules with typed public interfaces, explicit input/output contracts and useful error handling. Keep the current milestone focused; add abstractions and components when a concrete requirement justifies them.
- As each service is introduced, define configuration validation, resource bounds, timeouts, retry/idempotency behavior, structured logs, health checks and graceful shutdown where applicable. Verify failure paths and recovery procedures before claiming operational readiness.
- Document setup, configuration, architecture, operational procedures and known limitations in English. Keep exercises and Portuguese coaching separate from product-facing contracts. Record consequential architectural decisions and measured experiments with reproducible inputs; avoid documentation ceremony for routine edits.
- Run the applicable tests, Ruff and Black for each change and retain Pylint diagnostics. Keep local and CI checks aligned. A configured workflow is pending until its actual run succeeds; check the recorded milestone criteria before declaring completion.
- Keep source, manifests, configuration examples, dependency locks, shared editor settings, curated fixtures and reviewed experiment evidence in Git. Put generated datasets, runtime volumes and logs in `/.local/`, disposable outputs in `/artifacts/` or `/reports/`, and scratch work in `/tmp/`. Keep private credentials outside Git; use obvious placeholders in tracked examples. Encrypted secret manifests may be tracked only under the approved secrets workflow.
- Review ignore rules with `git check-ignore` for both excluded files and files that must remain visible. Do not blanket-ignore YAML, JSON, SQL, editor settings, lockfiles or all data directories. Public certificate fixtures matching private-key patterns require a narrow, reviewed exception. Git ignore rules do not replace secret scanning and do not remove files already tracked.

## Project Structure & Module Organization

The generator lives in `src/labflow/`, tests in `tests/`, and CI in `.github/workflows/`. Planned paths include `deploy/base/`, `deploy/overlays/{local,autoscaling}/`, `infra/helm/`, and `experiments/`. Create them when their milestone requires them. There is no frontend in the initial scope.

## Build, Test, and Development Commands

Use the project Python 3.14 `.venv` with LabFlow installed in editable mode. Run `.venv/bin/python -m pytest -v`, `.venv/bin/ruff check .`, and `.venv/bin/black --check --diff .` from the repository root. Prefix agent commands with `rtk proxy`. CI repeats these checks; expand it as components appear. Validate rendered Helm/Kustomize output before applying it. Use explicit cluster contexts and namespaces.

## Coding Style & Naming Conventions

Use four-space Python indentation, two-space YAML indentation, `snake_case` functions/modules, and `PascalCase` classes. Ruff is the linter with its upstream default rules; Black is the sole formatter with its default stable style (88 columns, no preview). Do not also run Ruff formatting or add rule overrides without an agreed reason. Keep tool versions pinned and the editor/CI on the same environment. Pylint provides default diagnostics in the editor and CI. Do not suppress E0401 or inject paths into tests to hide import problems. Install the `src` package into the selected environment; tests use pytest with plain assertions, `pytest.raises` for failures and parametrization for input cases. Preserve existing cases when migrating tests; use fixtures when shared setup is needed. Validate and pin dependencies before adoption. Keep changes focused and prefer descriptive names.

## Testing Guidelines

Name tests `tests/test_*.py`. Use deterministic fixtures and bounded workloads. Test PostgreSQL/Kafka integration, outbox failure windows, idempotency, retries, and duplicate persistence. Both duplicate-delivery and operational HPA experiments are completion gates. No coverage percentage is established; test critical behavior and report evidence without inventing results.

## Commit & Pull Request Guidelines

No Git history exists at bootstrap. Obtain the learner's explicit approval before creating commits. Adopt Conventional Commits, such as `docs: record milestone zero`. PRs should explain the change, validation, limitations, and related issues when applicable; include experiment evidence for operational changes.

## Configuration & Ownership

Helm owns infrastructure; Kustomize owns application manifests. HPA owns worker replicas in the autoscaling profile. Keep secrets outside Git. Consult current official documentation and Context7 for library/platform details. Verify ARM64 compatibility and resource budgets before installation. Paid resources, accounts, and public endpoints require explicit authorization.
