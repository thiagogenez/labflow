#!/usr/bin/env bash
# Run Pylint on every versioned Python file, from the directory of the package
# that holds it (the one with a pyproject.toml): run from anywhere else,
# Pylint takes the shared labflow package for the importing package's own
# code and reports an import order that Ruff, which reads each package's
# pyproject.toml, calls correct. Files outside every package, such as
# scripts, are linted from the repository root.
set -euo pipefail

pylint="$PWD/.venv/bin/pylint"
files() { git ls-files --cached --others --exclude-standard -- "$@"; }

status=0
packages="^$"
while IFS= read -r project; do
  dir="$(dirname "$project")"
  packages="$packages|^$dir/"
  echo "== $dir"
  # Chained with && because errexit does not apply inside a subshell on the
  # left of ||.
  (cd "$dir" && files '*.py' | xargs "$pylint") || status=1
done < <(files '*/pyproject.toml' | sort)

others="$(files '*.py' | grep -Ev "$packages" || true)"
if [[ -n "$others" ]]; then
  echo "== other Python files"
  xargs "$pylint" <<<"$others" || status=1
fi
exit "$status"
