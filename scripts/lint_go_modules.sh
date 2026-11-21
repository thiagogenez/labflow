#!/usr/bin/env bash
# Lint and check go.mod tidiness in every Go module. golangci-lint runs per
# module and also reports gofmt and goimports findings (.golangci.yml).
set -euo pipefail

status=0
while IFS= read -r mod; do
  dir="$(dirname "$mod")"
  echo "== $dir"
  # Chained with && because errexit does not apply inside a subshell on the
  # left of ||.
  (cd "$dir" && go mod tidy -diff && golangci-lint run ./...) || status=1
done < <(git ls-files --cached --others --exclude-standard -- '*/go.mod' | sort)
exit "$status"
