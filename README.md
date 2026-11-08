# LabFlow

LabFlow is a public Platform Engineering/SRE learning lab built around a small Python/FastAPI service that generates reports from reproducible synthetic data.

Clients submit jobs, workers perform real computation, and a result consumer persists authoritative state in PostgreSQL. Kafka carries job requests and compact results. The initial platform runs on Minikube on an ARM64 Mac; Oracle workers are a later extension.

## Current Status

Milestone zero is complete. The first M1 slice implements a bounded, deterministic sales generator, report aggregation, and 26 test cases across 14 test functions. Local tests, Ruff lint, Black formatting and Pylint checks pass. CI defines separate Python quality, secret scanning, and dependency audit jobs; remote execution remains pending. No API, database, broker, or Kubernetes resources have been implemented.

The first demonstrable version must later include both a duplicate-delivery experiment and an operational CPU-based HPA experiment. Twenty hours is a review budget, not proof of completion.

## First Slice: Reproducible Sales

`generate_sales(seed=42, rows=3)` returns immutable sale records with a product ID, quantity, and unit price in integer cents. Its first record is product 2, quantity 1, price 4,606 cents. Repeating the same seed and size reproduces the dataset for the recorded generator/runtime versions, even after another call with a different seed.

The `sales-v1` generator accepts 1-10,000 rows and seeds from 0 through 4,294,967,295. It rejects booleans, other non-integer inputs, and values outside these bounds before allocating rows. Each call uses its own random generator. The row limit bounds one call; concurrency and processing-cost controls belong to later slices.

`summarize_sales` aggregates sale records into `sales_count`, `units_sold`, and `revenue_cents`. The three sales above produce 3 records, 8 units, and 16,998 cents. An empty tuple produces zero for all three fields.

## Local Setup

Use a Python 3.14 virtual environment so the editor, tests and lint tools resolve the same installed `src` package:

```sh
uv venv --python python3.14 .venv
uv pip install --python .venv/bin/python --require-hashes --only-binary=:all: -r requirements-dev.txt
uv pip install --python .venv/bin/python --no-build-isolation --no-deps -e .
```

Open this repository as the VS Code workspace and select `.venv/bin/python`. Workspace settings point Pylint and Black at that environment. Installing the `src` package, rather than adding paths to individual test files, fixes runtime and linter imports together. The VS Code Pylint and Black Formatter extensions must be installed to use their editor integrations.

To regenerate the development lock after an intentional pin change:

```sh
uv pip compile --generate-hashes --universal --python-version 3.14 --only-binary :all: --no-header --no-annotate --output-file requirements-dev.txt requirements-dev.in
```

## Local Checks

Use Python 3.14.10, recorded in `.python-version`. The tests use pytest 9.2.0, pinned with its dependencies in the development requirements. Run these checks from the repository root:

| Command | Purpose |
| --- | --- |
| `.venv/bin/python -m pytest -v` | Run 26 cases, including report aggregation and individually named invalid inputs |
| `.venv/bin/ruff check --no-cache .` | Check default lint rules with Ruff 0.17.4 |
| `.venv/bin/black --check --diff .` | Verify default formatting with Black 26.10.1 |
| `.venv/bin/pylint --persistent=n src/labflow tests` | Check default Pylint diagnostics without writing its score cache |

Development tools and their transitive dependencies are pinned with hashes in `requirements-dev.txt`; direct pins live in `requirements-dev.in`. Ruff performs linting, Black owns formatting, and Pylint 4.1.3 provides editor and CI diagnostics. CI installs the package in editable mode, checks installed dependency consistency, and verifies that Python actually executes on ARM64.

The three CI jobs run on `ubuntu-24.04-arm` for pull requests and pushes to `main`. Each has read-only repository permissions; quality has a ten-minute timeout and each scanner has five minutes. Gitleaks 8.30.1 scans the fetched Git history with redacted findings. OSV-Scanner 2.7.0 checks the pinned Python dependencies for known vulnerabilities. Scanner binaries have explicit versions and SHA-256 checksums; actions have verified full commit references. Findings and scan failures fail their jobs. PR runs can cancel obsolete PR runs; each main commit gets its own run.

Local Gitleaks 8.30.1 scanning found no secrets in the 17 Git-visible files selected for the initial commit. Remote scanner execution remains pending. A successful audit only covers the scanned inputs and known detection rules/advisories. Required status checks must be configured in GitHub after the repository and job names exist; a workflow alone does not enforce merge restrictions.

The local experiment passed: identical inputs reproduced the expected data, a different seed changed the dataset, unrelated calls did not alter it, and invalid sizes/types were rejected. This is correctness evidence for the generator, not a throughput benchmark or evidence that the asynchronous application works.

## Start Here

- [Repository guidelines](AGENTS.md): contributor and learning workflow.
- [Implementation plan](docs/plan.md): architecture, milestones, ownership, experiments, and completion criteria.
- [Environment preflight](docs/preflight.md): observed facts, estimates, unresolved checks, and installation gates.
- [M1 compatibility review](docs/compatibility.md): selected tools, official sources, and remaining CI/dependency checks.

Repository artifacts are written in English. Learning discussions are in Portuguese.
