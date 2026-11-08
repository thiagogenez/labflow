#!/usr/bin/env bash
# Install the pinned OSV-Scanner Linux ARM64 release for GitHub Actions.
set -euo pipefail

cd "${RUNNER_TEMP:?RUNNER_TEMP must be set by the runner}"

curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 \
  --output osv-scanner \
  "https://github.com/google/osv-scanner/releases/download/v${OSV_VERSION}/osv-scanner_linux_arm64"
printf '%s  %s\n' "$OSV_SHA256" osv-scanner | sha256sum --check --strict
chmod +x osv-scanner
