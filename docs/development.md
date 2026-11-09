# Development

## Environment

Use CPython 3.14.10, recorded in [`.python-version`](../.python-version), and a repository-local `.venv`. The public package lives under `src/` and must be installed into the environment used by the editor, tests and lint tools.

```bash
make setup
```

Direct development-tool pins live in `requirements-dev.in`; the complete transitive resolution and hashes live in `requirements-dev.txt`. The build backend is also pinned in `pyproject.toml`. No global package installation is needed.

To update the lock after an intentional pin change:

```bash
uv pip compile --generate-hashes --universal --python-version 3.14 --only-binary :all: --no-header --no-annotate --output-file requirements-dev.txt requirements-dev.in
```

## Commit signatures

Sign every commit with the configured GPG or SSH identity. Keep `commit.gpgsign` enabled; do not disable signing or create unsigned fallback commits when the signing tool is unavailable.

The maintainer uses SSH signing through 1Password (`gpg.format=ssh` and `op-ssh-sign`). Reuse that configured identity and register its public key on GitHub as a **Signing key**. See [1Password commit signing](https://www.1password.dev/ssh/git-commit-signing).

Before merging, confirm that GitHub marks every incoming commit **Verified**. A signature stored locally is insufficient for this gate: register the corresponding public signing key and resolve verification errors first. Recheck signatures and CI after any history rewrite, and verify the resulting commit on `main`. See [GitHub signature verification](https://docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification).

## Commit quality gate

`make setup` installs the hook as part of onboarding. To reinstall only the hook after preparing the environment:

```bash
.venv/bin/python -m pre_commit install
```

The versioned `Makefile`, `.pre-commit-config.yaml`, tool locks and installer scripts define the same checks for every developer. The file generated at `.git/hooks/pre-commit` contains a clone-specific Python path and is local Git metadata; Git does not copy client hooks during cloning. Each developer explicitly activates them with `make setup`. See [Git hooks](https://git-scm.com/book/en/v2/Customizing-Git-Git-Hooks).

Every commit automatically runs **`make check`** through the installed pre-commit hook. The shared `.pre-commit-config.yaml` defines ShellCheck, Ruff, Black in check-only mode, Pylint and pytest. All indexed Python and shell files are checked, including `.github/scripts`, even for a documentation-only commit. Any nonzero result aborts the commit; no diagnostics are suppressed.

Stage changes to `.pre-commit-config.yaml` before committing; pre-commit refuses an unstaged configuration. The outer hook temporarily stashes other unstaged tracked changes and restores them afterward. Inside that staged view, `make check` selects all indexed files and the full test suite. The individual tools use pre-commit's `manual` stage, selected explicitly by Make; the entry-point hook uses the `pre-commit` stage. This separation prevents recursion. See [pre-commit stages](https://pre-commit.com/#confining-hooks-to-run-at-certain-stages).

After `make setup`, the normal workflow needs no separate check command:

```bash
git add <intended-files>
git commit -S -m "type: describe the change"
```

For earlier feedback, `make check` remains available. Run directly, it checks the **working-tree contents** of indexed paths without hiding unstaged edits. New files must be added to the index to be selected for ShellCheck, Ruff, Black or Pylint. Pytest separately runs the whole discovered test suite, including untracked tests if present. A passing working-tree check does not prove that a different staged version passed.

To preview exactly the hook's staged mode without creating a commit:

```bash
make check-staged
```

The CI job installs the same pinned tools in `.venv`, uses the same ShellCheck installer, and runs `--all-files` against its clean checkout, including every versioned shell script. The [pre-commit command reference](https://pre-commit.com/#pre-commit-run) describes both modes. Local hooks do not replace required CI status checks. The active main ruleset requires pull requests, signed commits and the three CI jobs, with no bypass actors.

## Editor

Open the repository root in VS Code and select `.venv/bin/python`. Shared [workspace settings](../.vscode/settings.json) select pytest discovery and point the Pylint and Black integrations at the same environment. Their respective VS Code extensions are required for those editor features.

When an import works in that environment but an editor diagnostic remains, check the selected interpreter and restart the relevant language/lint server before changing package paths. Avoid `sys.path` injection and disabling import diagnostics.

## Local checks

Run from the repository root:

| Command | Purpose |
| --- | --- |
| `.venv/bin/python -m pytest -v` | Named test cases, assertion details and failure summaries |
| `.venv/bin/ruff check --no-cache .` | Upstream default lint rules |
| `.venv/bin/black --check --diff .` | Default stable formatting without edits |
| `make check` | Check working-tree contents of Git-indexed shell and Python files, including `.github/scripts`, followed by pytest |
| `make check-staged` | Run the full gate in the staged view, temporarily preserving unstaged tracked changes |
| `uv pip check --python .venv/bin/python` | Installed dependency consistency |
| `actionlint .github/workflows/ci.yml` | Workflow syntax and supported expressions |
| `.venv/bin/shellcheck .github/scripts/*.sh` | Default ShellCheck diagnostics using the pinned local binary |

Use Black to apply formatting. Keep Ruff's formatter disabled so one tool owns the formatting policy. Add fixtures and integration dependencies when their milestone introduces the corresponding service.

The current reporting suite has 26 cases across 14 functions. Parametrization names invalid input cases separately; this count is not a coverage percentage. The known sales test supplies its own records so report aggregation is checked independently of generation.

## ShellCheck installation

ShellCheck 0.11.0 is installed into `.venv/bin` by the versioned `.github/scripts/install-shellcheck.sh`. Local setup and CI call the same installer. It selects the official release for macOS/Linux on ARM64 or x86_64, verifies its platform-specific SHA-256 before extraction, and needs no global package-manager changes or Docker daemon. Unsupported platforms, missing environments, download errors and checksum mismatches fail setup. The checksums are recorded in the installer and were verified against the upstream release metadata.

## CI boundaries

The workflow uses `ubuntu-24.04-arm`, the Python version in `.python-version`, full action commit references, read-only repository permissions and bounded runtimes. Scanner installers in [`.github/scripts/`](../.github/scripts/) use strict Bash error handling, HTTPS downloads and fixed SHA-256 verification.

Gitleaks scans fetched Git history; OSV-Scanner audits the explicitly selected Python lockfile. Findings and scan errors fail their jobs. A passing scan covers its selected inputs and detection rules or known advisories, not every possible vulnerability.

The shared quality gate passed [PR CI](https://github.com/thiagogenez/labflow/actions/runs/42521136229) and [main CI](https://github.com/thiagogenez/labflow/actions/runs/42521372612) at `2ff07d8`, including native ARM64 execution, ShellCheck and the Python checks. Changes introduced afterward require their own validation. Required status checks are not yet configured.

Historical prepublication scans covered an earlier set of files. Re-scan all current commit candidates after README, diagram, script or configuration changes before publishing. Keep unique evidence and historical results in the compatibility record without treating old scans as current.

## README illustrations

The README displays the animated GIF directly at a maximum width of 720 pixels and links to a static PNG. The 840×320 illustration previews the **planned** Kafka/worker path: three distinct colored jobs, three worker pods, compact results, a result consumer and PostgreSQL. Jobs and results are topics in one Kafka cluster. This is an illustrative topology and schedule, not a runtime trace; the initial deployment will start with one worker. Regenerate both assets with:

```bash
.venv/bin/python .github/scripts/render_readme_assets.py
```

Pillow is a hash-pinned development dependency, separate from application runtime dependencies. The generator draws the illustration locally, without an external image API or downloaded fonts. Font availability can change rasterization between machines. Check that jobs enter distinct workers, workers return results through Kafka, and only the result consumer writes to PostgreSQL. Review opening, concurrent-processing, result-delivery and completed frames at desktop and narrow widths. Keep the static alternative and descriptive alt text; GIF playback does not automatically follow reduced-motion preferences.

The **How it works** section distinguishes the running Python core from the planned distributed system. Its Mermaid diagram remains editable in the README. Keep both the architecture labels and numbered explanation consistent with [the plan](plan.md).

## Changes and reviews

- Keep one coherent behavior or maintenance improvement per change.
- Include tests for meaningful behavior and failure paths; preserve the deterministic generator contract unless explicitly versioning it.
- Record which checks actually ran and disclose pending integration or remote execution.
- Review changes to CI permissions, dependencies, credential handling and deployment ownership explicitly.
- Follow [AGENTS.md](../AGENTS.md) for project conventions and the guided learning workflow. Creating a commit requires the learner's explicit approval.

## Further documentation

- [Implementation plan](plan.md): milestones, architecture and experiment criteria.
- [Environment preflight](preflight.md): observations, estimates and outstanding capacity checks.
- [Compatibility record](compatibility.md): selected versions and validation history.
