# Milestone Zero: Environment Preflight

Status: M0 complete for the first bounded Python implementation. Initial read-only checks ran on 2026-11-08 at approximately 13:45 UTC; follow-up checks and learner-supplied output established the facts below. No stack components were installed during M0. Runtime capacity estimates remain hypotheses to validate during execution.

## Observed, Reported, and Unknown

| Item | Evidence | Status |
| --- | --- | --- |
| Repository scope | All agent commands ran in LabFlow; no Git metadata exists | Rechecked |
| Contributor guide | `AGENTS.md` was absent at initial bootstrap and preserved throughout M0 | Its command/style sections were updated with the actual M1 implementation afterward |
| Host architecture | `arm64` | Observed |
| macOS | 27.0.1 | Observed |
| Python interpreters available on host | Default `python3`: 3.15.2; explicit `python3.14`: 3.14.10; explicit `python3.13`: 3.13.18 | Observed |
| M1 Python candidate | CPython 3.14.10 ran natively on ARM64; stdlib `venv` and SSL are available | Limited runtime check passed; project environment not created |
| Ruff | 0.17.4; isolated, cache-free stdin check accepted `py314` and returned no findings during M0 | M1 subsequently passed project lint and formatting checks with the same existing binary |
| pytest | Not on PATH and not installed in the inspected Python 3.14 environment | 9.2.0 researched as candidate; not installed or executed |
| uv | 0.13.7, aarch64-apple-darwin | Observed; no environment/dependency changes made |
| Docker client | 29.9.3 | Observed; kept separate from the server version |
| Docker server | 29.9.2, `linux`, `aarch64`, 12 CPUs | Evidence supplied by learner's successful command |
| kubectl client | 1.36.1, darwin/arm64 | Observed; cluster compatibility unknown |
| Embedded Kustomize | 5.8.1 from kubectl client output | Observed; overlays not implemented |
| Other command availability | `rtk` found; Git 2.56.0 and GitHub CLI 2.103.3 | Local versions observed; M1 connector lookup returned 404 for the intended repository and no local remote is configured |
| Commands not found on PATH | `minikube`, `helm`, standalone `kustomize`, `argocd`, `tailscale` | Not proof that no GUI or alternative installation exists |
| Host resources | `hw.memsize=38654705664`, `hw.physicalcpu=12`, `hw.logicalcpu=12`: 36 GiB RAM and 12 physical/logical CPUs | Verified from learner-supplied sysctl output; M3 Pro model remains learner-reported |
| Docker total memory | 18,830,053,376 bytes = 17.536853790283203 GiB, rounded to 17.54 GiB | Daemon output supplied by learner; total, not free memory |
| Agent access to Docker usage metrics | `docker stats --no-stream` failed with socket permission denied | Learner-side sample recorded below |
| Learner-side container snapshot | Empty `docker stats --no-stream` output at approximately 15:08 Europe/Berlin; learner confirms no error | Consistent with no running containers sampled; not proof of free VM memory |
| Docker Desktop resource settings | 12 CPUs, 18,432 MiB memory, 2,048 MiB swap, 488,281 MiB disk limit | Read-only inspection of resource fields only; settings were not changed |
| Host filesystem free space | Approximately 995.64 GiB at check time | Observed; not Docker VM free space or a reserved allocation |
| Docker disk image metadata | 476.837 GiB logical size; 206.041 GiB host-allocated blocks; backing filesystem has approximately 995.64 GiB free | File metadata only; guest filesystem free space not measured |
| Cluster, charts, images and CNI | Not selected or tested | Pending |
| Oracle capacity and connectivity | Not checked | Deferred to Oracle phase |

The original failed agent Docker query printed empty fields and zero resource values before reporting permission denied. Those values are invalid and are superseded by the successful learner-side evidence below. The client and server versions are different observations; no update was attempted.

```text
Server=29.9.2 OS=linux Arch=aarch64 CPUs=12 MemoryBytes=18830053376
```

The conversion is `18830053376 / 1024^3 = 17.536853790283203 GiB`. Docker reports the daemon's total resources, not idle CPU or currently free memory. The earlier approximately 18 GiB assumption is now replaced by this measured total.

No credentials, registry configuration, kubeconfig contents, or private account details were collected.

The empty container snapshot was explicitly confirmed to have finished without error. It is a point-in-time observation, not a measurement of whole-VM memory or future peak usage. Docker disk image allocation is likewise not the same as live filesystem usage inside the guest. Storage evidence is sufficient for the small M1 source/test files; guest capacity checks belong before starting stateful components.

## Proposed Resource Envelope

These are estimates to validate, not benchmark results or final pod requests/limits. The proposed starting point is one Minikube node with up to 8 vCPUs and 14 GiB memory, conditional on actual Docker resources and competing workloads.

| Component group | Estimated memory allowance |
| --- | ---: |
| Kubernetes, CNI, DNS and Metrics Server | 2 GiB |
| Single Kafka broker/controller | 2 GiB |
| PostgreSQL | 1 GiB |
| API, outbox publisher and result consumer | 1 GiB |
| Up to two local workers | 2 GiB |
| Prometheus, Grafana, Alertmanager and exporters | 2.5 GiB |
| Traefik | 0.25 GiB |
| Load generator | 0.5 GiB |
| Initial component allowance | 11.25 GiB |
| Later Argo CD allowance, including its internal cache | 1.25 GiB |
| Allowance including later Argo CD | 12.5 GiB |

Against the daemon's reported 17.54 GiB total, a 14 GiB node would leave approximately 3.54 GiB outside Minikube, before other containers and VM overhead are accounted for. This is arithmetic headroom, not observed free memory. An 8-vCPU node similarly leaves a nominal four of the daemon's 12 CPUs outside its proposed ceiling, not a guarantee that those CPUs are idle or reserved.

Within the node, component allowances must leave room for transient startup and rollout usage. Validate allocatable resources and the sum of requests at maximum worker replicas before enabling HPA; API replicas, generators and other containers share the same physical capacity. The 8-vCPU/14-GiB proposal remains a hypothesis to measure as components are introduced. Exact stack consumption and whole-VM peak headroom are not prerequisites for the bounded Python-only M1 step.

Propose at least 50 GiB of available Docker storage for data, metrics, images and logs, then validate retention and actual usage. Host filesystem free space is not a Docker disk guarantee. PVC size declarations must be checked against the provisioner's real capacity enforcement. Do not treat host-path storage as replicated or a backup.

Oracle is excluded from this budget. Its quota and available capacity must be verified separately before use.

## Version and Installation Gates

For each component, record applicable runtime/chart versions, image digests, official compatibility sources, ARM64 evidence, execution evidence, and estimated/measured resources before calling that component validated. The [M1 compatibility review](compatibility.md) records Python 3.14.10, pytest 9.2.0 and the pinned development tools adopted after this preflight. The foundation subsequently passed PR and main CI, including native Linux ARM64 execution. M1 pins Python, dependency artifacts and CI action references. No infrastructure charts or images are selected yet. Avoid `latest` as a version policy.

Use the explicit Python 3.14 interpreter rather than relying on the current `python3` default. M0 itself did not install packages. The completed M1 foundation uses a repository-local `.venv` and an editable package installation; follow the [development guide](development.md) for current setup. FastAPI, the Kafka client, database driver, and their Linux/ARM64 dependencies must be checked before M2 adoption; M1 tool checks do not validate the complete stack or a container image.

Before cluster setup, validate Minikube, Kubernetes, Docker driver, CNI, Helm, embedded Kustomize and Metrics Server compatibility. The default Minikube network plugin does not establish NetworkPolicy enforcement; choose a capable CNI and test actual traffic behavior. [Minikube network policies](https://minikube.sigs.k8s.io/docs/handbook/network_policy/)

Before external access, verify the local/private routing path, DNS, certificate names and trust. Check secret encryption at rest explicitly; base64 is only an encoding. [Kubernetes Secret guidance](https://kubernetes.io/docs/concepts/security/secrets-good-practices/)

## Milestone Zero Exit Checklist

- [x] Check for an existing `AGENTS.md` before writing; none was present.
- [x] Recheck and preserve the existing `AGENTS.md` during the follow-up.
- [x] Record approved architecture, basic CI from first code, both required experiments, and Kustomize ownership in the plan.
- [x] Separate observed environment facts, learner reports, estimates, and unresolved checks.
- [x] Record successful Docker server communication, Linux/ARM64 architecture, CPUs, and total memory from learner-supplied output.
- [x] Research the next-stage Python/Ruff/pytest candidate set using official sources and check existing Python/Ruff execution without installation.
- [x] Record the learner's empty container snapshot without claiming it proves available memory.
- [x] Inspect Docker's configured disk limit, disk-image file allocation, and backing filesystem free space without starting containers or changing settings.
- [x] Confirm host RAM and CPU figures from learner-side sysctl output: 36 GiB RAM, 12 physical CPUs, 12 logical CPUs.
- [x] Identify the first-code candidate versions and existing execution evidence; package installation/tests and CI execution belong to M1, not this preflight.
- [x] Close M0 with verified facts, actual next-step blockers, and deferred component checks; proceed to the already authorized first M1 slice without another approval round.

## Closeout and Deferred Checks

Verified facts: host ARM64, 36 GiB RAM and 12 physical/logical CPUs; Docker Linux/ARM64 with 12 CPUs and 17.54 GiB total memory; an empty container snapshot without a reported error; approximately 995.64 GiB host filesystem free space; configured Docker disk limit and disk-image file allocation; working Python 3.14.10 and Ruff 0.17.4. No further infrastructure measurement blocks the bounded Python-only M1 slice. Do not repeat these preflight requests.

A single container snapshot cannot prove sustained peak headroom and does not include all VM/kernel overhead; the Docker CLI also treats memory cache specially. [Docker stats reference](https://docs.docker.com/reference/cli/docker/container/stats/)

Guest filesystem free space, Kubernetes allocatable resources, runtime image compatibility, actual stack consumption, and peak headroom can wait until their components are executed. The resource budget remains an experiment hypothesis. M0 itself created documentation only; the subsequently authorized M1 slice adds application code, tests and a CI definition.

The agent's Docker socket access remains denied. A later package-resolution attempt also encountered a DNS failure reaching PyPI. Neither prevents the initial standard-library tests from running. Project dependency installation and remote CI access are M1 execution concerns, not reasons to reopen the infrastructure preflight.
