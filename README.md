# LabFlow

**Reproducible workloads. Compact results. Observable operations.**

[![CI](https://github.com/thiagogenez/labflow/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/thiagogenez/labflow/actions/workflows/ci.yml)
[![Python 3.14](https://img.shields.io/badge/Python-3.14-3776AB?style=flat-square&logo=python&logoColor=white)](pyproject.toml)
[![Tests: pytest](https://img.shields.io/badge/tests-pytest-0A9EDC?style=flat-square)](tests/)
[![Lint: Ruff](https://img.shields.io/badge/lint-Ruff-253047?style=flat-square)](pyproject.toml)
[![Style: Black](https://img.shields.io/badge/style-Black-111827?style=flat-square)](pyproject.toml)
[![License: MIT](https://img.shields.io/badge/license-MIT-0D9488?style=flat-square)](LICENSE)

[Quick start](#-quick-start) · [How it works](#-how-it-works) · [Quality](#-quality-and-ci) · [Roadmap](#-roadmap) · [Development](docs/development.md)

LabFlow turns reproducible synthetic sales into compact reports. It is a public Platform Engineering and SRE lab for learning to operate asynchronous workloads, investigate failures and measure recovery.

**Running today:** a bounded Python generator and report aggregator. **Next:** durable jobs, asynchronous processing and observable Kubernetes operations.

<img src="docs/assets/labflow-flow.gif" width="720" alt="Planned Kubernetes worker flow: three distinct jobs from Kafka partitions go to three worker pods. Each worker generates data and computes a compact result, which returns through Kafka to the result consumer and PostgreSQL.">

*Planned processing flow, illustrated with three jobs and worker pods. Jobs and results are topics in the same Kafka cluster; motion does not represent measured performance. [Static diagram](docs/assets/labflow-flow.png).*

## 🧪 Current capabilities

| Capability | State | Evidence |
| --- | --- | --- |
| Deterministic sales generation | Implemented | Seeded generation, bounded inputs and isolated random state |
| Report aggregation | Implemented | Counts, units and revenue in integer cents; empty-input behavior |
| Unit tests | Passed locally and in ARM64 CI | 26 pytest cases across 14 functions |
| Python quality checks | Passed locally and in ARM64 CI | Ruff, Black and Pylint |
| CI and security jobs | Shared gate verified on ARM64 | Python/ShellCheck checks, Gitleaks and OSV-Scanner passed on PR #2 and `main` |
| API, Kafka and PostgreSQL | Planned | Job lifecycle, transactional outbox and result persistence |
| Kubernetes and observability | Planned | Helm, Kustomize, Traefik, Prometheus, Grafana and Alertmanager |
| GitOps and remote capacity | Later milestones | Argo CD, GHCR and Oracle-hosted workers |

The shared quality gate passed [PR CI](https://github.com/thiagogenez/labflow/actions/runs/42521136229) and [main CI](https://github.com/thiagogenez/labflow/actions/runs/42521372612) at `2ff07d8`. Later changes require their own checks; the live badge tracks the current `main` workflow.

## ⚡ Quick start

**Prerequisites:** Git, Make, CPython 3.14.10 and uv on macOS or Linux. The implemented core runs directly in a Python environment; Docker and Kubernetes enter in later milestones.

```bash
git clone https://github.com/thiagogenez/labflow.git
cd labflow
make setup
```

Generate a reproducible dataset and calculate its report using the project environment:

```bash
.venv/bin/python - <<'PY'
from labflow.reporting import summarize_sales
from labflow.synthetic import generate_sales

sales = generate_sales(seed=42, rows=3)
report = summarize_sales(sales)
print(report)
PY
```

Expected output:

```text
{'sales_count': 3, 'units_sold': 8, 'revenue_cents': 16998}
```

`make setup` installs the pinned development environment, downloads a checksum-verified ShellCheck binary into `.venv/bin` and activates the shared commit checks for that clone.

Run the test suite from the repository root:

```bash
.venv/bin/python -m pytest -v
```

### Calculation contract

- `generate_sales` accepts **1–10,000 rows** and an integer seed from **0 through 4,294,967,295**. Booleans, other types and out-of-range inputs are rejected.
- Each call has its own random generator. Retain the seed, row count, generator version (`sales-v1`) and Python version when recording an experiment.
- `summarize_sales` receives sale records and returns `sales_count`, `units_sold` and `revenue_cents`. Empty input returns three zeros.
- Prices and revenue use integer cents. The input limit bounds one generation call; aggregate concurrency and job-cost controls are planned separately.

## ⚙️ How it works

### Today: the reporting core

1. **Generate:** [`generate_sales`](src/labflow/synthetic.py) validates the seed and row limit, then creates the same sales for the same versioned inputs.
2. **Calculate:** each sale exposes `total_cents = quantity × unit_price_cents`. [`summarize_sales`](src/labflow/reporting.py) counts sales, adds units and sums those totals.
3. **Return:** the caller receives a dictionary containing three integers. There is no API, queue or database in this execution path yet.

These functions will execute inside each worker. The quick start exercises them directly; the animation previews the future distributed processing path.

### Target: durable, asynchronous jobs

**Planned architecture.** Kubernetes will run the application core in **Minikube on the Mac**. A separate **K3s cluster on an Oracle ARM VM** will add workers later. These services and clusters are not implemented yet.

```mermaid
flowchart TB
    client["Client / load generator"]

    subgraph core["Kubernetes cluster · Minikube / Mac"]
        subgraph entry["HTTP entry"]
            ingress["Traefik<br/>Ingress controller + TLS"]
            api_service["API Service"]
            api["FastAPI pods<br/>Deployment"]
            ingress --> api_service --> api
        end

        subgraph storage["Persistent services"]
            db[("PostgreSQL<br/>official state · PVC")]
            kafka[("Kafka<br/>jobs + results topics · PVC")]
        end

        relay["Outbox publisher<br/>Deployment"]
        worker["Worker pods<br/>Deployment · generate + compute"]
        writer["Result consumer<br/>Deployment"]

        api -->|"job + outbox transaction / status queries"| db
        db -->|"committed outbox"| relay
        relay -->|"publish jobs"| kafka
        kafka -->|"consume job partitions"| worker
        worker -->|"publish compact results"| kafka
        kafka -->|"consume results"| writer
        writer -->|"deduplicate + persist"| db
    end

    client <-->|"HTTPS · submit job / query result"| ingress

    subgraph remote["Later · Kubernetes / K3s on Oracle ARM"]
        extra["Additional worker pods<br/>Deployment"]
    end

    kafka -.->|"Tailscale · consume jobs"| extra
    extra -.->|"Tailscale · publish results"| kafka

    classDef service fill:#eff6ff,stroke:#2563eb,color:#172554
    classDef data fill:#ecfdf5,stroke:#0d9488,color:#134e4a
    classDef future fill:#faf5ff,stroke:#9333ea,color:#581c87,stroke-dasharray:5 5
    class api,relay,worker,writer,ingress,api_service service
    class db,kafka data
    class extra future
```

1. **Accept a job.** The API will record the job and its outbox event in one PostgreSQL transaction, then return a job identifier. Polling will read status and results from PostgreSQL.
2. **Publish the work.** The outbox publisher will send committed events to Kafka. A crash after publishing can cause redelivery; accepted jobs must not silently disappear between the database write and publication.
3. **Distribute jobs across workers.** Kafka will assign job partitions across pods in one worker consumer group. Each worker will generate its dataset from small, versioned inputs and calculate the report locally. The animation shows three partitions and three workers as an example; the first deployment starts with one worker.
4. **Return the outcome.** Workers will publish a result before committing their input offsets. Local and future Oracle workers will share the same job consumer group.
5. **Persist once.** The result consumer will deduplicate final effects in PostgreSQL before committing result offsets. Computation may repeat; system-wide exactly-once processing is not a guarantee.

Oracle workers will have no direct database access. Tailscale will connect the environments privately; Kafka, PostgreSQL and administrative interfaces will not be exposed directly to the internet. One Mac remains one physical failure domain.

### Kubernetes operation

| Responsibility | Planned resources and behavior |
| --- | --- |
| HTTP routing | An Ingress defines routing and TLS configuration. Traefik is its controller; the API Service selects ready FastAPI pods. |
| Application lifecycle | Deployments manage API, publisher, worker and result-consumer pods, with health probes, resource requests and limits. |
| Persistent data | PostgreSQL and Kafka use PersistentVolumeClaims (PVCs). Provisioner behavior must be checked before multinode experiments; local volumes do not provide independent-host recovery. |
| Isolation | Namespaces, ResourceQuota, LimitRange, ServiceAccounts/RBAC and NetworkPolicies enforced by the selected CNI. |
| Worker autoscaling | Metrics Server → resource metrics API → CPU HPA → worker Deployment replica count. CPU requests define the utilization baseline. |

HTTP load balancing distributes requests across API pods; Kafka distributes partitions across consumers. HPA adds worker pods within available cluster capacity. More pods do not add physical CPU or memory, and useful consumer parallelism depends on the partition count. Oracle capacity is activated separately and manually at first. See [Kubernetes CPU autoscaling](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/) for the metrics path.

### Reliability by design

These are implementation and experiment requirements for the distributed milestones:

| Concern | Intended mechanism | Required verification |
| --- | --- | --- |
| Job admission | Job and outbox event in one database transaction | Crash around publication without silently dropping accepted work |
| Duplicate delivery | At-least-once processing with transactional result deduplication | Redeliver a result after a database commit and verify one authoritative result |
| Retries | Durable attempt state, bounded retries and backoff | Exhaustion, stale attempts and malformed events |
| Scaling | CPU-based HPA with Metrics Server and sufficient Kafka partitions | Observe additional available workers receiving work under controlled load |
| Resource sharing | Requests, limits, quotas and admission bounds | Measure pressure, backlog and competing workloads |
| Recovery | Off-Mac backups, restore procedures and fencing | Measure data loss, duplicate effects, RPO and RTO |

The initial deployment has a single physical host. High availability, provider-loss recovery and performance improvements remain experiment outcomes to establish.

### Observability and delivery

Planned observability follows a clear path: application and infrastructure metrics → **Prometheus** → **Grafana dashboards** and **Alertmanager notifications**. Experiments will correlate CPU, memory, restarts, queue lag, throughput and job-completion latency. Job identifiers belong in logs; metric labels remain bounded.

**Helm** will manage infrastructure components. **Kustomize** will define application bases and local/autoscaling overlays. **Argo CD** and digest-based image promotion through **GHCR** enter in the GitOps milestone. Initial HPA scaling uses Metrics Server; Prometheus provides operational visibility.

## 🛠 Quality and CI

The [CI workflow](.github/workflows/ci.yml) defines three independent jobs:

| Job | Checks |
| --- | --- |
| Python quality and tests | Hashed development environment, package/dependency consistency and native ARM64 checks, then the same pre-commit ShellCheck/Ruff/Black/Pylint/pytest gates used locally against every versioned Python file |
| Secret scan | Full-history Gitleaks scanning with redacted findings |
| Dependency audit | OSV-Scanner audit of pinned Python dependencies |

Jobs have read-only repository permissions and explicit timeouts. Actions are pinned to full commit references; scanner downloads have fixed versions and SHA-256 checksums. PR runs can cancel obsolete runs. Publishing and deployment credentials are outside this CI scope.

The local commit hook blocks staged Python and shell failures, including errors in documentation and CI scripts. CI runs the same hook configuration with `--all-files`. Scanner installation logic lives in [`.github/scripts/`](.github/scripts/). Required merge checks must be configured in GitHub after their first runs. See [development and validation](docs/development.md) for local checks and the limits of each result.

## 🗺 Roadmap

- [x] Bounded, reproducible reporting core and unit tests.
- [x] Local quality tooling and CI definitions.
- [x] Complete successful foundation PR and `main` CI runs.
- [ ] Add the API, PostgreSQL, outbox, Kafka and workers.
- [ ] Run on local Kubernetes with Helm and Kustomize.
- [ ] Establish observability and reproduce the duplicate-delivery failure window.
- [ ] Measure fixed-worker versus CPU-autoscaling behavior.
- [ ] Add GitOps delivery, rollback and image promotion.
- [ ] Extend worker capacity to Oracle and verify backup/restore procedures.
- [ ] Explore Redis caching, replicated profiles and network-partition behavior.

Each milestone requires execution evidence and an explanation of observed behavior. The [implementation plan](docs/plan.md) defines the acceptance criteria; configuration files alone do not complete an operational milestone.

## Repository map

```text
.github/
  workflows/ci.yml             CI jobs and permissions
  scripts/                     Scanner installers and README asset generation
docs/
  assets/                      Animated and static flow illustrations
  development.md               Local setup, checks and contribution workflow
  plan.md                      Architecture, milestones and experiments
  preflight.md                 Environment observations and capacity assumptions
  compatibility.md             Tool versions and validation history
Makefile                       Developer setup and quality commands
.pre-commit-config.yaml         Shared commit and CI checks
src/labflow/
  synthetic.py                 Deterministic sale records
  reporting.py                 Compact report aggregation
tests/                         Unit and parametrized input tests
```

## 🤝 Contributing

Keep changes focused, include relevant tests and run the checks in [the development guide](docs/development.md). Document behavior changes and operational assumptions. Use fictitious data and configuration placeholders; keep credentials and employer material outside the repository. Contributor and agent conventions are in [AGENTS.md](AGENTS.md).

## License

Licensed under the [MIT License](LICENSE).

Copyright © 2026 Thiago Augusto Lopes Genez.
