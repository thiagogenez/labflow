# M1 Compatibility Review

Reviewed on 2026-11-08 using current official documentation, Context7, package metadata, and existing tools. Updated with local M1 execution evidence. This does not validate the future Kubernetes stack.

## First-Code Versions and Evidence

| Component | Candidate | Evidence available | Remaining validation |
| --- | --- | --- | --- |
| CPython | 3.14.10 | Pinned in `.python-version`; executed natively on ARM64; 11 generator unit tests passed | Execute the workflow on GitHub |
| Ruff | 0.17.4 | Exact version and ARM64 wheel hashes pinned; actual project lint and formatting checks passed using the existing local binary | Verify artifact installation and execution in remote CI |
| unittest | Bundled with Python 3.14.10 | Runs all initial tests without third-party packages | Add real-service integration checks as components appear |
| pytest | 9.2.0, researched only | Package metadata reviewed during M0 | Deferred; not a dependency of the first slice |

Python 3.14 remains in bugfix maintenance according to the [Python version status table](https://devguide.python.org/versions/). The [3.14.10 release](https://www.python.org/downloads/release/python-31410/) matches the interpreter already available locally. This makes it a supported candidate with local execution evidence; it is not a claim that every future client library works with it.

The default `python3` currently selects 3.15.2. Local checks explicitly use `python3.14`; CI reads `.python-version`. The first tests use only the standard library. When local package installation is needed, use an isolated `.venv`; none was created during M0 or this first slice. [Python virtual environments](https://docs.python.org/3.14/library/venv.html)

## ARM64 Evidence and Limits

The Python 3.14 check reported `arm64` and build platform `macosx-27.0-arm64`. Ruff's [0.17.4 release files](https://pypi.org/project/ruff/0.17.4/) include `macosx_11_0_arm64` and `manylinux_2_17_aarch64.manylinux2014_aarch64` wheels. The existing local binary accepted the explicit Python 3.14 target without writing a cache or project configuration.

The [pytest 9.2.0 package](https://pypi.org/project/pytest/9.2.0/) publishes a platform-independent Python wheel. That metadata does not prove that all eventual dependencies, plugins, CI runners, or container images execute correctly on ARM64.

The sandbox could not resolve PyPI during package compilation. The first slice therefore uses standard-library unittest instead of requiring pytest installation. Existing tests remain compatible with pytest. Ruff's two wheel hashes were verified against the official release metadata and recorded explicitly; no successful artifact download or new package installation is claimed. No Linux/ARM64 application image was built, pulled, inspected, or executed.

## Existing Tools and Selection Boundary

Git 2.56.0, GitHub CLI 2.103.3, and uv 0.13.7 are available. Their presence does not prove GitHub authentication, remote repository setup, or successful CI. uv is inventory evidence, not an additional required project dependency at this stage.

pytest, FastAPI, confluent-kafka, and psycopg distribution metadata were absent from the inspected Python 3.14 environment. No global packages were changed. The pytest candidate remains source-reviewed rather than execution-validated.

The first workflow uses `ubuntu-24.04-arm`, documented as a standard ARM64 runner for public repositories, with explicit Python 3.14.10 and ARM64 architecture. [GitHub-hosted runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)

Verified action tags resolve to these commit SHAs, and their `action.yml` input definitions were inspected at those commits:

- [actions/checkout v7.0.1](https://github.com/actions/checkout/tree/3d3c42e5aac5ba805825da76410c181273ba90b1): `3d3c42e5aac5ba805825da76410c181273ba90b1`.
- [actions/setup-python v7.1.0](https://github.com/actions/setup-python/tree/6d4fd22b20465ccc0f7354476588e8df7c9ca0e0): `6d4fd22b20465ccc0f7354476588e8df7c9ca0e0`.

Workflow syntax passes the locally available actionlint. Exact interpreter installation and workflow execution on GitHub remain unverified. No Git repository or remote is configured locally; a connector lookup of the intended LabFlow repository returned 404, which does not distinguish absence from restricted access. The first slice is locally verified, while full M1 CI completion remains pending.

## Checks Assigned to Later Components

- Before M2 dependencies: confirm FastAPI, the selected Kafka client, PostgreSQL driver, and their transitive dependencies against the chosen Python runtime and Linux/ARM64. Retain evidence before pinning them.
- Before M3 cluster setup: select compatible Minikube, Kubernetes, Docker driver, Helm, CNI, provisioner, and Metrics Server versions; verify images and capacity. The observed kubectl 1.36.1 and embedded Kustomize 5.8.1 are client observations, not a selected server version.
- Before later infrastructure: validate each chart, image, support status, and resource envelope when introducing Argo CD, Oracle/K3s, or application Redis. Do not preapprove an entire stack from the first Python tool checks.

The scope and acceptance gates remain in [the implementation plan](plan.md); environment facts and unresolved capacity checks remain in [preflight](preflight.md).


## Quality and Import Configuration Update (2026-11-08)

The preceding no-install and Ruff formatter observations describe the initial slice, before this update. Current policy uses Ruff 0.17.4 for upstream default lint rules, Black 26.10.1 for default stable formatting, Pylint 4.1.3 for editor diagnostics, and setuptools 84.2.1 as the explicit build backend. Direct and transitive development dependencies are hash-pinned. No global Python packages are changed.

The original E0401 was reproduced with the global Pylint. Setting only `source-roots=src` did not resolve it. `pyproject.toml` now declares the `src` package and editable installation into the Python 3.14 `.venv` provides the import path. Editor settings use that environment. Full Pylint cleanup and its blocking CI gate remain pending; E0401 is not disabled in project configuration.

References: [Python src layout](https://packaging.python.org/en/latest/discussions/src-layout-vs-flat-layout/), [setuptools package discovery](https://setuptools.pypa.io/en/latest/userguide/package_discovery.html), [Ruff defaults](https://docs.astral.sh/ruff/configuration/), [Black defaults](https://black.readthedocs.io/en/stable/usage_and_configuration/the_basics.html), and [Pylint editor settings](https://github.com/microsoft/vscode-pylint#settings).

Validation after installation: all 12 unit tests passed with `PYTHONPATH` removed, including the learner's unchanged `total_cents` test; Black left all three Python files unchanged; actionlint passed; the Pylint import-error-only diagnostic passed. The full Pylint scan reports 13 missing-docstring conventions in tests, while Ruff's upstream defaults report four SIM117 nested-context findings. Both sets remain visible and pending; Ruff is still a blocking CI check. The source and tests were preserved byte for byte during configuration. Remote GitHub CI has not run. Black required an execution permission adjustment for its local multiprocessing socket; no style change was needed.

Follow-up validation on 2026-11-08: the learner updated the nested test contexts, resolving the four SIM117 findings. Added 13 missing module/class/method docstrings at the learner's request; comparison of the parsed code with docstrings removed confirms unchanged executable structure. All 12 unit tests, full Pylint, Ruff and Black now pass locally. This supersedes the pending local findings above; the Pylint CI gate and remote workflow execution remain pending.

## Pytest Migration (2026-11-08)

At the learner's request, pytest 9.2.0 replaces unittest as the project runner and test API. The original 12 test functions and their input cases are retained; parametrization exposes 24 separately named cases instead of grouping boundary and invalid inputs into unittest subtests. Assertions use plain `assert`; expected exception types and message checks use `pytest.raises(..., match=...)`. The production generator is unchanged. Earlier unittest and no-pytest observations describe historical checks. CI, editor discovery and current commands now use pytest; remote CI execution is still pending.

Pytest migration validation: installed pytest 9.2.0 and its hash-pinned dependencies in the Python 3.14 environment; all 24 cases passed with PYTHONPATH removed. Ruff, Black, full Pylint and actionlint passed. The production generator was preserved byte for byte. Parametrized cases retain the original boundary values, invalid types, expected exception types and message patterns.

## Publication CI Preparation

Report aggregation adds two tests, bringing the suite to 26 cases across 14 functions. Git is now initialized on `main` with no commits or remote. The learner confirmed GitHub CLI authentication; commit creation requires their explicit approval.

CI adds Pylint, installed-dependency consistency checking, a native ARM64 runtime assertion, and separate secret/dependency scanner jobs. Binary versions and hashes were checked against the official GitHub release API:

| Artifact | SHA-256 |
| --- | --- |
| [Gitleaks 8.30.1 macOS ARM64 archive](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1) | `b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5` |
| Gitleaks 8.30.1 Linux ARM64 archive | `e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080` |
| [OSV-Scanner 2.7.0 Linux ARM64 binary](https://github.com/google/osv-scanner/releases/tag/v2.7.0) | `4061ef4c021ed3c47f203b818f337e351336cc5cd67c0274fc36bd1dfc728dd7` |

These entries are release metadata checks; local execution evidence is recorded below. Remote execution of all CI jobs remains pending. The scanners use no publishing credentials. Gitleaks redacts findings; OSV reads the explicitly selected hashed requirements file using its pip requirements parser. Its findings and lookup errors must fail CI. No vulnerability exceptions are configured. [Gitleaks commands](https://github.com/gitleaks/gitleaks/blob/v8.30.1/README.md), [OSV source scanning](https://github.com/google/osv-scanner/blob/v2.7.0/docs/scan-source.md).

Local validation of this CI preparation passed: 26 pytest cases, Ruff, all five Python files checked individually with Black, Pylint with its score cache disabled, and actionlint. `uv pip check` found the 20 installed packages compatible; CI will use `python -m pip check`. Black's multi-file process manager and uv's default external cache are restricted in this sandbox, so local checks used individual Black invocations and an ignored workspace uv cache. No style or diagnostic rules were relaxed.

The learner installed Gitleaks through Homebrew; its executable reports 8.30.1. The agent scanned temporary copies of all 17 files selected by `git ls-files --cached --others --exclude-standard`, with default rules, full redaction, inline allow comments disabled, and a 120-second timeout. Gitleaks exited with code 0 and reported no leaks. The temporary snapshot was removed. Ignored environments, caches and local credentials were outside this publication scan. There is no commit history yet, so this is source-file scanning; CI will scan the fetched Git history. Any change to the selected files requires a fresh scan before publication.

## Repository-wide Python Gate (2026-11-08)

Added pre-commit 4.7.0 and the shared `.pre-commit-config.yaml`. The installed local hook runs Ruff, Black in check-only mode, Pylint for staged Python files including tooling scripts, and pytest. CI now creates the project `.venv` and executes the same configuration with `--all-files`, covering every versioned Python file rather than a hard-coded source/test list. The `unsupported` hook language is the current upstream name for hooks using the existing environment, formerly `system`.

Before this PR split, verified the actual hook in an isolated temporary Git repository containing six Python files, including a documentation generator reserved for the next PR: the full baseline passed; a staged tooling script with a Pylint R0913 failure was rejected even with a corrected unstaged working copy; the working copy was restored; staging the correction passed. No commit was created for this regression check. All 26 existing pytest cases passed, along with Ruff, Black, Pylint, pre-commit configuration validation and actionlint. Local hook installation succeeded in LabFlow. Remote execution of this updated workflow remains pending; GitHub branch rules are required to make status checks mandatory for merging.

## Shared Onboarding and ShellCheck Gate (2026-11-08)

The versioned Makefile now provides `make setup`, `make check`, `make check-staged` and `make test`. Setup installs the pinned Python environment, the repository-local ShellCheck binary and the generated Git hook. The clone-specific `.git/hooks/pre-commit` launcher is not versioned; its shared behavior is defined by `.pre-commit-config.yaml`, the Makefile, dependency locks and installer scripts. No global Git hook configuration is changed.

Added ShellCheck to the shared hook configuration and the CI installation sequence. Local setup and the ARM64 runner use `.github/scripts/install-shellcheck.sh` to install ShellCheck 0.11.0 into `.venv/bin`, verifying the platform-specific SHA-256 against pinned upstream release values before extraction. Upstream metadata was checked for macOS/Linux ARM64 and x86_64 archives; actual local execution was verified on macOS ARM64. The shellcheck-py wheel distribution was deliberately not adopted because its current release lacks a Linux ARM64 wheel and the project requires binary-only dependency installation.

Verified setup in the existing checkout and in a fresh, independent temporary repository/environment. The fresh setup installed and activated the hook; the complete repository gate, including all three shell scripts and the 26 pytest cases, passed. A staged shell script with unquoted expansion was blocked with SC2086 even when its working copy was corrected; the unstaged correction was restored, and staging the correction passed. No commit was created. Actionlint, ShellCheck, Bash syntax checking and whitespace checks passed. The Linux runner execution and GitHub required-check enforcement remain separate pending verification/configuration.

## Foundation CI and Signature Verification (2026-11-08)

[PR CI](https://github.com/thiagogenez/labflow/actions/runs/42516722176) and [main CI](https://github.com/thiagogenez/labflow/actions/runs/42517878193) passed at `c7c3faeedb447ace1cf15d018e7594fa4b04916c`, covering clean dependency installation, native Linux ARM64 execution, 26 pytest cases, Python quality checks, Gitleaks and OSV-Scanner. GitHub reports valid signatures for all three history commits. PR #1 was integrated by fast-forward, preserving the SSH-signed commits. These runs precede the shared pre-commit and ShellCheck workflow changes; they do not validate those changes.

## Public Documentation Assets (2026-11-08)

The initial README asset generator was outside the former `src/labflow tests` Pylint scope. Reproduced C0103 (hyphenated module name), E0401 (Pillow absent from the project environment), R0913 and R0917 (six label-helper arguments). Renamed it to `.github/scripts/render_readme_assets.py`, grouped label styling into a typed dataclass, and added Pillow 12.3.0 to the hash-pinned development dependencies. No diagnostics or exit statuses were suppressed. The generated GIF and PNG remain byte-for-byte identical.


## Shared Quality Gate CI (2026-11-08)

[PR #2 CI](https://github.com/thiagogenez/labflow/actions/runs/42521136229) and [main CI](https://github.com/thiagogenez/labflow/actions/runs/42521372612) passed at `2ff07d8643529b34a15baf73e9d64ff7a9788da7`. All three jobs passed: the shared ShellCheck/Python gate, full-history Gitleaks and OSV-Scanner. The quality job installed the pinned environment, checked dependency consistency and confirmed native Linux ARM64 execution. GitHub reports the original 1Password SSH signature as Verified; PR #2 was integrated by fast-forward. This resolves the earlier pending Linux-runner verification for the shared gate. Required GitHub status checks remain unconfigured. These runs precede the public documentation/Pillow changes, which require their own validation.
