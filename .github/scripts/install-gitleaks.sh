#!/usr/bin/env bash
# Install the pinned Gitleaks Linux ARM64 release for GitHub Actions.
set -euo pipefail

cd "${RUNNER_TEMP:?RUNNER_TEMP must be set by the runner}"

curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 \
  --output gitleaks.tar.gz \
  "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_arm64.tar.gz"
printf '%s  %s\n' "$GITLEAKS_SHA256" gitleaks.tar.gz | sha256sum --check --strict
tar -xzf gitleaks.tar.gz gitleaks
